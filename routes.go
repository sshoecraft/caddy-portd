package portd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const adminBase = "http://localhost:2019"

var httpClient = &http.Client{Timeout: 5 * time.Second}

func adminRequest(method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, adminBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", adminBase)
	return httpClient.Do(req)
}

// FindHTTPServer returns the name of the first configured HTTP server.
func FindHTTPServer() (string, error) {
	resp, err := adminRequest("GET", "/config/apps/http/servers", nil)
	if err != nil {
		return "", fmt.Errorf("failed to query servers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var servers map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&servers); err != nil {
		return "", fmt.Errorf("failed to decode servers: %w", err)
	}

	for name := range servers {
		return name, nil
	}
	return "", fmt.Errorf("no HTTP servers configured")
}

// AddRoute adds a reverse proxy route for a service to the Caddy HTTP server.
func AddRoute(svc *ServiceEntry) error {
	server, err := FindHTTPServer()
	if err != nil {
		return err
	}

	// Remove any existing route with this ID first (idempotent)
	RemoveRoute(svc.Name)

	route := buildRouteJSON(svc)
	routeBytes, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("failed to marshal route: %w", err)
	}

	path := fmt.Sprintf("/config/apps/http/servers/%s/routes", server)
	resp, err := adminRequest("POST", path, bytes.NewReader(routeBytes))
	if err != nil {
		return fmt.Errorf("failed to add route: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to add route (status %d): %s", resp.StatusCode, body)
	}

	return nil
}

// RemoveRoute removes a service's route from Caddy by its @id.
func RemoveRoute(name string) error {
	id := "portd_" + name
	resp, err := adminRequest("DELETE", "/id/"+id, nil)
	if err != nil {
		return fmt.Errorf("failed to remove route: %w", err)
	}
	defer resp.Body.Close()

	// 200 = removed, other status = not found or error (both acceptable)
	return nil
}

// AddLandingRoute adds the portd landing page route at /.
func AddLandingRoute() error {
	server, err := FindHTTPServer()
	if err != nil {
		return err
	}

	// Remove existing landing route first
	resp, err := adminRequest("DELETE", "/id/portd_landing", nil)
	if err == nil {
		resp.Body.Close()
	}

	route := buildLandingRouteJSON()
	routeBytes, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("failed to marshal landing route: %w", err)
	}

	path := fmt.Sprintf("/config/apps/http/servers/%s/routes", server)
	resp, err = adminRequest("POST", path, bytes.NewReader(routeBytes))
	if err != nil {
		return fmt.Errorf("failed to add landing route: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("failed to add landing route (status %d): %s", resp.StatusCode, body)
	}

	return nil
}

func buildRouteJSON(svc *ServiceEntry) map[string]interface{} {
	dial := fmt.Sprintf("%s:%d", svc.Host, svc.Port)

	handlers := []interface{}{}

	if svc.StripPrefix {
		handlers = append(handlers, map[string]interface{}{
			"handler":           "rewrite",
			"strip_path_prefix": "/" + svc.Name,
		})
	}

	handlers = append(handlers, map[string]interface{}{
		"handler":   "reverse_proxy",
		"upstreams": []map[string]string{{"dial": dial}},
	})

	return map[string]interface{}{
		"@id": "portd_" + svc.Name,
		"match": []map[string]interface{}{
			{"path": []string{"/" + svc.Name, "/" + svc.Name + "/*"}},
		},
		"handle":   handlers,
		"terminal": true,
	}
}

func buildLandingRouteJSON() map[string]interface{} {
	return map[string]interface{}{
		"@id": "portd_landing",
		"match": []map[string]interface{}{
			{"path": []string{"/"}},
		},
		"handle": []map[string]interface{}{
			{"handler": "portd_landing"},
		},
		"terminal": true,
	}
}
