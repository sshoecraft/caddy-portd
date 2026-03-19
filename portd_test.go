package portd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

func testLogger() *zap.Logger {
	logger, _ := zap.NewDevelopment()
	return logger
}

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"), testLogger())

	entry := &ServiceEntry{
		Name:         "test-svc",
		Port:         8080,
		Host:         "127.0.0.1",
		StripPrefix:  true,
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	}
	reg.Register(entry)

	got := reg.Get("test-svc")
	if got == nil {
		t.Fatal("expected to find registered service")
	}
	if got.Port != 8080 {
		t.Errorf("expected port 8080, got %d", got.Port)
	}
}

func TestRegistryDeregister(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"), testLogger())

	entry := &ServiceEntry{
		Name:         "test-svc",
		Port:         8080,
		Host:         "127.0.0.1",
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	}
	reg.Register(entry)
	reg.Deregister("test-svc")

	if reg.Get("test-svc") != nil {
		t.Error("expected service to be deregistered")
	}
}

func TestRegistryList(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"), testLogger())

	for _, name := range []string{"charlie", "alpha", "bravo"} {
		reg.Register(&ServiceEntry{
			Name:         name,
			Port:         8080,
			Host:         "127.0.0.1",
			Healthy:      true,
			RegisteredAt: time.Now().UTC(),
		})
	}

	list := reg.List()
	if len(list) != 3 {
		t.Fatalf("expected 3 services, got %d", len(list))
	}
	// Should be sorted alphabetically
	if list[0].Name != "alpha" || list[1].Name != "bravo" || list[2].Name != "charlie" {
		t.Errorf("expected alphabetical order, got %s, %s, %s", list[0].Name, list[1].Name, list[2].Name)
	}
}

func TestRegistryPersistence(t *testing.T) {
	dir := t.TempDir()
	persistFile := filepath.Join(dir, "registry.json")

	// Register and save
	reg1 := NewRegistry(persistFile, testLogger())
	reg1.Register(&ServiceEntry{
		Name:         "persist-test",
		Port:         3000,
		Host:         "127.0.0.1",
		StripPrefix:  true,
		HealthURI:    "/health",
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	})

	// Load in a new registry
	reg2 := NewRegistry(persistFile, testLogger())
	if err := reg2.Load(); err != nil {
		t.Fatalf("failed to load: %v", err)
	}

	got := reg2.Get("persist-test")
	if got == nil {
		t.Fatal("expected to find persisted service")
	}
	if got.Port != 3000 {
		t.Errorf("expected port 3000, got %d", got.Port)
	}
	if got.HealthURI != "/health" {
		t.Errorf("expected health_uri /health, got %s", got.HealthURI)
	}
}

func TestRegistryLoadNonExistent(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "nonexistent.json"), testLogger())
	if err := reg.Load(); err != nil {
		t.Errorf("loading non-existent file should not error, got: %v", err)
	}
}

func TestRegistryHealthTracking(t *testing.T) {
	reg := NewRegistry(filepath.Join(t.TempDir(), "registry.json"), testLogger())
	reg.Register(&ServiceEntry{
		Name:         "health-test",
		Port:         8080,
		Host:         "127.0.0.1",
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	})

	// Increment failures
	for i := 1; i <= 3; i++ {
		count := reg.IncrementFail("health-test")
		if count != i {
			t.Errorf("expected fail count %d, got %d", i, count)
		}
	}

	// Mark healthy again
	reg.SetHealthy("health-test", true)
	got := reg.Get("health-test")
	if !got.Healthy {
		t.Error("expected service to be healthy")
	}
	if got.FailCount != 0 {
		t.Errorf("expected fail count 0, got %d", got.FailCount)
	}
}

func TestNamePattern(t *testing.T) {
	valid := []string{"solar", "my-app", "app_v2", "Grafana", "test123"}
	invalid := []string{"", "-start", "_start", "has space", "special!", "/path", ".dot"}

	for _, name := range valid {
		if !namePattern.MatchString(name) {
			t.Errorf("expected %q to be valid", name)
		}
	}
	for _, name := range invalid {
		if namePattern.MatchString(name) {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestBuildRouteJSON(t *testing.T) {
	svc := &ServiceEntry{
		Name:        "solar",
		Port:        8080,
		Host:        "127.0.0.1",
		StripPrefix: true,
	}

	route := buildRouteJSON(svc)

	if route["@id"] != "portd_solar" {
		t.Errorf("expected @id portd_solar, got %v", route["@id"])
	}
	if route["terminal"] != true {
		t.Error("expected terminal to be true")
	}

	matchers, ok := route["match"].([]map[string]interface{})
	if !ok || len(matchers) != 1 {
		t.Fatal("expected one matcher set")
	}
	paths, ok := matchers[0]["path"].([]string)
	if !ok || len(paths) != 2 {
		t.Fatalf("expected 2 path patterns, got %v", matchers[0]["path"])
	}
	if paths[0] != "/solar" || paths[1] != "/solar/*" {
		t.Errorf("unexpected paths: %v", paths)
	}

	handlers, ok := route["handle"].([]interface{})
	if !ok || len(handlers) != 2 {
		t.Fatalf("expected 2 handlers (rewrite + reverse_proxy), got %d", len(handlers))
	}
}

func TestBuildRouteJSONNoStrip(t *testing.T) {
	svc := &ServiceEntry{
		Name:        "grafana",
		Port:        3000,
		Host:        "127.0.0.1",
		StripPrefix: false,
	}

	route := buildRouteJSON(svc)
	handlers, ok := route["handle"].([]interface{})
	if !ok || len(handlers) != 1 {
		t.Fatalf("expected 1 handler (reverse_proxy only), got %d", len(handlers))
	}
}

func TestRegistryPersistFileDir(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "deep", "nested", "registry.json")
	reg := NewRegistry(nested, testLogger())
	reg.Register(&ServiceEntry{
		Name:         "test",
		Port:         80,
		Host:         "127.0.0.1",
		Healthy:      true,
		RegisteredAt: time.Now().UTC(),
	})

	// Verify the nested directory was created
	if _, err := os.Stat(filepath.Dir(nested)); os.IsNotExist(err) {
		t.Error("expected nested directory to be created")
	}
}
