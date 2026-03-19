package portd

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

var (
	activeAppMu sync.RWMutex
	activeApp   *PortdApp
)

func init() {
	caddy.RegisterModule(PortdApp{})
	caddy.RegisterModule(PortdAdmin{})
	caddy.RegisterModule(LandingHandler{})
	httpcaddyfile.RegisterGlobalOption("portd", parsePortdCaddyfile)
}

// PortdApp is the main Caddy app module for portd.
type PortdApp struct {
	PersistFile    string         `json:"persist_file,omitempty"`
	HealthInterval caddy.Duration `json:"health_interval,omitempty"`

	registry *Registry
	health   *HealthChecker
	logger   *zap.Logger
}

func (PortdApp) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "portd",
		New: func() caddy.Module { return new(PortdApp) },
	}
}

func (app *PortdApp) Provision(ctx caddy.Context) error {
	app.logger = ctx.Logger()
	app.registry = NewRegistry(app.PersistFile, app.logger)

	interval := time.Duration(app.HealthInterval)
	if interval == 0 {
		interval = 30 * time.Second
	}
	app.health = NewHealthChecker(app.registry, interval, app.logger)

	return nil
}

func (app *PortdApp) Start() error {
	activeAppMu.Lock()
	activeApp = app
	activeAppMu.Unlock()

	if err := app.registry.Load(); err != nil {
		app.logger.Warn("failed to load persisted registry", zap.Error(err))
	}

	go app.restoreRoutes()
	app.health.Start()

	app.logger.Info("portd started",
		zap.String("persist_file", app.PersistFile),
		zap.Duration("health_interval", time.Duration(app.HealthInterval)),
	)
	return nil
}

func (app *PortdApp) Stop() error {
	app.health.Stop()

	activeAppMu.Lock()
	activeApp = nil
	activeAppMu.Unlock()

	app.logger.Info("portd stopped")
	return nil
}

func (app *PortdApp) restoreRoutes() {
	// Wait for HTTP server to become available
	var serverFound bool
	for i := 0; i < 20; i++ {
		server, err := FindHTTPServer()
		if err == nil && server != "" {
			serverFound = true
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !serverFound {
		app.logger.Warn("no HTTP server found, cannot restore routes")
		return
	}

	services := app.registry.List()
	for _, svc := range services {
		if err := AddRoute(svc); err != nil {
			app.logger.Error("failed to restore route",
				zap.String("name", svc.Name),
				zap.Error(err),
			)
		} else {
			app.logger.Info("restored route", zap.String("name", svc.Name))
		}
	}

	if err := AddLandingRoute(); err != nil {
		app.logger.Warn("failed to add landing page route", zap.Error(err))
	}
}

// PortdAdmin provides the admin API endpoints for portd.
type PortdAdmin struct{}

func (PortdAdmin) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "admin.api.portd",
		New: func() caddy.Module { return new(PortdAdmin) },
	}
}

func (PortdAdmin) Routes() []caddy.AdminRoute {
	return []caddy.AdminRoute{
		{Pattern: "/portd/register", Handler: caddy.AdminHandlerFunc(handleRegister)},
		{Pattern: "/portd/deregister", Handler: caddy.AdminHandlerFunc(handleDeregister)},
		{Pattern: "/portd/services", Handler: caddy.AdminHandlerFunc(handleServices)},
	}
}

func getApp() (*PortdApp, error) {
	activeAppMu.RLock()
	app := activeApp
	activeAppMu.RUnlock()
	if app == nil {
		return nil, caddy.APIError{
			HTTPStatus: http.StatusServiceUnavailable,
			Err:        fmt.Errorf("portd not configured or not started"),
		}
	}
	return app, nil
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

func handleRegister(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodPost {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed")}
	}

	app, err := getApp()
	if err != nil {
		return err
	}

	var req struct {
		Name        string `json:"name"`
		Port        int    `json:"port"`
		Host        string `json:"host,omitempty"`
		StripPrefix *bool  `json:"strip_prefix,omitempty"`
		HealthURI   string `json:"health_uri,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("invalid request body: %v", err)}
	}

	if req.Name == "" {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("name is required")}
	}
	if !namePattern.MatchString(req.Name) {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("name must be alphanumeric with hyphens or underscores")}
	}
	if req.Port < 1 || req.Port > 65535 {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("port must be between 1 and 65535")}
	}

	if req.Host == "" {
		req.Host = "127.0.0.1"
	}
	stripPrefix := true
	if req.StripPrefix != nil {
		stripPrefix = *req.StripPrefix
	}

	entry := &ServiceEntry{
		Name:         req.Name,
		Port:         req.Port,
		Host:         req.Host,
		StripPrefix:  stripPrefix,
		HealthURI:    req.HealthURI,
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	}

	// Check for existing registration
	existing := app.registry.Get(req.Name)
	if existing != nil {
		if existing.Port == req.Port && existing.Host == req.Host {
			// Idempotent re-registration: same name+port+host, refresh it
			existing.RegisteredAt = time.Now().UTC()
			existing.Healthy = true
			existing.StripPrefix = stripPrefix
			existing.HealthURI = req.HealthURI
			if saveErr := app.registry.Save(); saveErr != nil {
				app.logger.Warn("failed to persist registry", zap.Error(saveErr))
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"status": "registered",
				"name":   req.Name,
				"url":    "/" + req.Name + "/",
			})
			return nil
		}
		return caddy.APIError{
			HTTPStatus: http.StatusConflict,
			Err:        fmt.Errorf("service '%s' already registered on port %d", req.Name, existing.Port),
		}
	}

	// Register in our registry
	app.registry.Register(entry)

	// Add reverse proxy route to Caddy
	if err := AddRoute(entry); err != nil {
		app.registry.Deregister(req.Name)
		return caddy.APIError{
			HTTPStatus: http.StatusInternalServerError,
			Err:        fmt.Errorf("failed to add route: %v", err),
		}
	}

	app.logger.Info("service registered",
		zap.String("name", req.Name),
		zap.Int("port", req.Port),
		zap.String("host", req.Host),
	)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "registered",
		"name":   req.Name,
		"url":    "/" + req.Name + "/",
	})
	return nil
}

func handleDeregister(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodDelete {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed")}
	}

	app, err := getApp()
	if err != nil {
		return err
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("invalid request body: %v", err)}
	}
	if req.Name == "" {
		return caddy.APIError{HTTPStatus: http.StatusBadRequest, Err: fmt.Errorf("name is required")}
	}

	if app.registry.Get(req.Name) == nil {
		return caddy.APIError{HTTPStatus: http.StatusNotFound, Err: fmt.Errorf("service '%s' not registered", req.Name)}
	}

	// Remove route from Caddy (ignore errors — route may already be gone)
	if removeErr := RemoveRoute(req.Name); removeErr != nil {
		app.logger.Warn("failed to remove route (may already be removed)",
			zap.String("name", req.Name),
			zap.Error(removeErr),
		)
	}

	app.registry.Deregister(req.Name)

	app.logger.Info("service deregistered", zap.String("name", req.Name))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "deregistered",
		"name":   req.Name,
	})
	return nil
}

func handleServices(w http.ResponseWriter, r *http.Request) error {
	if r.Method != http.MethodGet {
		return caddy.APIError{HTTPStatus: http.StatusMethodNotAllowed, Err: fmt.Errorf("method not allowed")}
	}

	app, err := getApp()
	if err != nil {
		return err
	}

	services := app.registry.List()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"services": services,
	})
	return nil
}

// LandingHandler serves the service directory page at /.
type LandingHandler struct{}

func (LandingHandler) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers.portd_landing",
		New: func() caddy.Module { return new(LandingHandler) },
	}
}

var landingTmpl = template.Must(template.New("landing").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<title>Services</title>
<style>
  body { font-family: system-ui, -apple-system, sans-serif; max-width: 800px; margin: 40px auto; padding: 0 20px; color: #333; }
  h1 { border-bottom: 2px solid #eee; padding-bottom: 10px; }
  table { border-collapse: collapse; width: 100%; margin-top: 20px; }
  th, td { text-align: left; padding: 10px 14px; border-bottom: 1px solid #eee; }
  th { background: #fafafa; font-weight: 600; }
  tr:hover { background: #f5f8ff; }
  a { color: #0066cc; text-decoration: none; }
  a:hover { text-decoration: underline; }
  .healthy { color: #22863a; }
  .unhealthy { color: #cb2431; }
  .empty { color: #666; font-style: italic; margin-top: 20px; }
</style>
</head>
<body>
<h1>Services</h1>
{{if .}}
<table>
<tr><th>Service</th><th>Backend</th><th>Status</th><th>Registered</th></tr>
{{range .}}
<tr>
  <td><a href="/{{.Name}}/">{{.Name}}</a></td>
  <td>{{.Host}}:{{.Port}}</td>
  <td class="{{if .Healthy}}healthy{{else}}unhealthy{{end}}">{{if .Healthy}}healthy{{else}}unhealthy{{end}}</td>
  <td>{{.RegisteredAt.Format "2006-01-02 15:04:05 UTC"}}</td>
</tr>
{{end}}
</table>
{{else}}
<p class="empty">No services registered.</p>
{{end}}
</body>
</html>`))

func (h LandingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	activeAppMu.RLock()
	app := activeApp
	activeAppMu.RUnlock()

	if app == nil {
		return next.ServeHTTP(w, r)
	}

	services := app.registry.List()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return landingTmpl.Execute(w, services)
}

// Caddyfile global option parser
func parsePortdCaddyfile(d *caddyfile.Dispenser, existingVal interface{}) (interface{}, error) {
	app := &PortdApp{}
	if existingVal != nil {
		var ok bool
		app, ok = existingVal.(*PortdApp)
		if !ok {
			return nil, d.Errf("existing portd config has wrong type")
		}
	}

	for d.Next() {
		for d.NextBlock(0) {
			switch d.Val() {
			case "persist_file":
				if !d.Args(&app.PersistFile) {
					return nil, d.ArgErr()
				}
			case "health_interval":
				var interval string
				if !d.Args(&interval) {
					return nil, d.ArgErr()
				}
				dur, err := caddy.ParseDuration(interval)
				if err != nil {
					return nil, d.Errf("invalid duration: %v", err)
				}
				app.HealthInterval = caddy.Duration(dur)
			default:
				return nil, d.Errf("unrecognized portd option: %s", d.Val())
			}
		}
	}

	return httpcaddyfile.App{
		Name:  "portd",
		Value: caddyconfig.JSON(app, nil),
	}, nil
}

// Interface guards
var (
	_ caddy.Module                = (*PortdApp)(nil)
	_ caddy.App                   = (*PortdApp)(nil)
	_ caddy.Provisioner           = (*PortdApp)(nil)
	_ caddy.Module                = (*PortdAdmin)(nil)
	_ caddy.AdminRouter           = (*PortdAdmin)(nil)
	_ caddy.Module                = (*LandingHandler)(nil)
	_ caddyhttp.MiddlewareHandler = (*LandingHandler)(nil)
)
