package portd

import (
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const maxFailCount = 3

// HealthChecker periodically checks registered services and manages their routes.
type HealthChecker struct {
	registry *Registry
	interval time.Duration
	logger   *zap.Logger
	client   *http.Client
	stopCh   chan struct{}
	done     chan struct{}
}

func NewHealthChecker(registry *Registry, interval time.Duration, logger *zap.Logger) *HealthChecker {
	return &HealthChecker{
		registry: registry,
		interval: interval,
		logger:   logger,
		client:   &http.Client{Timeout: 5 * time.Second},
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
}

func (hc *HealthChecker) Start() {
	go hc.run()
}

func (hc *HealthChecker) Stop() {
	close(hc.stopCh)
	<-hc.done
}

func (hc *HealthChecker) run() {
	defer close(hc.done)

	ticker := time.NewTicker(hc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-hc.stopCh:
			return
		case <-ticker.C:
			hc.checkAll()
		}
	}
}

func (hc *HealthChecker) checkAll() {
	services := hc.registry.List()
	for _, svc := range services {
		if svc.HealthURI == "" {
			continue
		}
		hc.checkService(svc)
	}
}

func (hc *HealthChecker) checkService(svc *ServiceEntry) {
	url := fmt.Sprintf("http://%s:%d%s", svc.Host, svc.Port, svc.HealthURI)

	resp, err := hc.client.Get(url)
	if err != nil {
		hc.handleFailure(svc)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		hc.handleSuccess(svc)
	} else {
		hc.handleFailure(svc)
	}
}

func (hc *HealthChecker) handleSuccess(svc *ServiceEntry) {
	if !svc.Healthy {
		hc.logger.Info("service recovered, restoring route",
			zap.String("name", svc.Name),
		)
		hc.registry.SetHealthy(svc.Name, true)

		if err := AddRoute(svc); err != nil {
			hc.logger.Error("failed to restore route for recovered service",
				zap.String("name", svc.Name),
				zap.Error(err),
			)
		}
	} else {
		// Reset fail count on success
		hc.registry.SetHealthy(svc.Name, true)
	}
}

func (hc *HealthChecker) handleFailure(svc *ServiceEntry) {
	count := hc.registry.IncrementFail(svc.Name)

	if count >= maxFailCount && svc.Healthy {
		hc.logger.Warn("service unhealthy, removing route",
			zap.String("name", svc.Name),
			zap.Int("consecutive_failures", count),
		)
		hc.registry.SetHealthy(svc.Name, false)

		if err := RemoveRoute(svc.Name); err != nil {
			hc.logger.Error("failed to remove route for unhealthy service",
				zap.String("name", svc.Name),
				zap.Error(err),
			)
		}
	}
}
