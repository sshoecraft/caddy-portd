package portd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"
)

// ServiceEntry represents a registered service.
type ServiceEntry struct {
	Name         string    `json:"name"`
	Port         int       `json:"port"`
	Host         string    `json:"host"`
	StripPrefix  bool      `json:"strip_prefix"`
	HealthURI    string    `json:"health_uri,omitempty"`
	Healthy      bool      `json:"healthy"`
	RegisteredAt time.Time `json:"registered_at"`

	// Health check tracking (not persisted)
	FailCount int `json:"-"`
}

// Registry manages service registrations with thread-safe access and persistence.
type Registry struct {
	mu          sync.RWMutex
	services    map[string]*ServiceEntry
	persistFile string
	logger      *zap.Logger
}

func NewRegistry(persistFile string, logger *zap.Logger) *Registry {
	if persistFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "/tmp"
		}
		persistFile = filepath.Join(home, ".config", "portd", "registry.json")
	}
	return &Registry{
		services:    make(map[string]*ServiceEntry),
		persistFile: persistFile,
		logger:      logger,
	}
}

func (reg *Registry) Register(entry *ServiceEntry) {
	reg.mu.Lock()
	reg.services[entry.Name] = entry
	reg.mu.Unlock()
	if err := reg.Save(); err != nil {
		reg.logger.Warn("failed to persist registry", zap.Error(err))
	}
}

func (reg *Registry) Deregister(name string) {
	reg.mu.Lock()
	delete(reg.services, name)
	reg.mu.Unlock()
	if err := reg.Save(); err != nil {
		reg.logger.Warn("failed to persist registry", zap.Error(err))
	}
}

func (reg *Registry) Get(name string) *ServiceEntry {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.services[name]
}

func (reg *Registry) List() []*ServiceEntry {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	entries := make([]*ServiceEntry, 0, len(reg.services))
	for _, entry := range reg.services {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name < entries[j].Name
	})
	return entries
}

func (reg *Registry) SetHealthy(name string, healthy bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if entry, ok := reg.services[name]; ok {
		entry.Healthy = healthy
		if healthy {
			entry.FailCount = 0
		}
	}
}

func (reg *Registry) IncrementFail(name string) int {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if entry, ok := reg.services[name]; ok {
		entry.FailCount++
		return entry.FailCount
	}
	return 0
}

func (reg *Registry) Save() error {
	reg.mu.RLock()
	data, err := json.MarshalIndent(reg.services, "", "  ")
	reg.mu.RUnlock()
	if err != nil {
		return err
	}

	dir := filepath.Dir(reg.persistFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(reg.persistFile, data, 0644)
}

func (reg *Registry) Load() error {
	data, err := os.ReadFile(reg.persistFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var services map[string]*ServiceEntry
	if err := json.Unmarshal(data, &services); err != nil {
		return err
	}

	reg.mu.Lock()
	reg.services = services
	if reg.services == nil {
		reg.services = make(map[string]*ServiceEntry)
	}
	reg.mu.Unlock()

	reg.logger.Info("loaded persisted registry", zap.Int("count", len(services)))
	return nil
}
