package healthchecker

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
	"github.com/cloudresty/corridor/proxy"
)

// HealthChecker is responsible for monitoring the health of upstream targets.
type HealthChecker struct {
	services    []config.ServiceConfig
	proxy       *proxy.Proxy // To call UpdateTargetHealth
	httpClient  *http.Client
	stopChan    chan struct{}
	wg          sync.WaitGroup
	statusStore *targetStatusStore
}

// targetStatus holds the current health state of an individual upstream target.
type targetStatus struct {
	targetURL            string
	serviceName          string
	config               *config.HealthCheckConfig
	consecutiveFailures  int
	consecutiveSuccesses int
	isHealthy            bool
	mu                   sync.Mutex
}

// targetStatusStore manages the health status of all targets being checked.
type targetStatusStore struct {
	statuses map[string]*targetStatus // Key: serviceName + "|" + targetURL
	mu       sync.RWMutex
}

func newTargetStatusStore() *targetStatusStore {
	return &targetStatusStore{
		statuses: make(map[string]*targetStatus),
	}
}

func (s *targetStatusStore) get(serviceName, targetURL string) *targetStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statuses[serviceName+"|"+targetURL]
}

func (s *targetStatusStore) set(serviceName, targetURL string, status *targetStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses[serviceName+"|"+targetURL] = status
}

// NewHealthChecker creates a new HealthChecker instance.
func NewHealthChecker(cfg *config.Config, p *proxy.Proxy) *HealthChecker {

	if p == nil {
		logging.Fatal("HealthChecker: Proxy instance cannot be nil.") // Or handle more gracefully
	}

	return &HealthChecker{
		services: cfg.Services,
		proxy:    p,
		httpClient: &http.Client{
			// Timeout for health check requests should be less than the interval.
			// This will be overridden by individual health check timeouts.
			Timeout: 5 * time.Second,
		},
		stopChan:    make(chan struct{}),
		statusStore: newTargetStatusStore(),
	}

}

// Start begins the health checking routines for all configured services.
func (hc *HealthChecker) Start() {

	logging.Info("HealthChecker: Starting health checks...")

	for i := range hc.services {

		serviceCfg := hc.services[i] // Create a local copy for the goroutine

		if serviceCfg.LoadBalancing != nil && serviceCfg.LoadBalancing.HealthChecks != nil && len(serviceCfg.LoadBalancing.Targets) > 0 {

			hcConfig := serviceCfg.LoadBalancing.HealthChecks
			logging.Debugf("HealthChecker: Setting up health checks for service %s with interval %ds", serviceCfg.Name, hcConfig.Interval)

			for j := range serviceCfg.LoadBalancing.Targets {

				targetCfg := serviceCfg.LoadBalancing.Targets[j] // Create a local copy

				// Initialize status for this target
				status := &targetStatus{
					targetURL:   targetCfg.URL,
					serviceName: serviceCfg.Name,
					config:      hcConfig,
					isHealthy:   true, // Assume healthy initially
				}
				hc.statusStore.set(serviceCfg.Name, targetCfg.URL, status)
				// Initial update to proxy (important if proxy starts with targets marked unhealthy by default)
				hc.proxy.UpdateTargetHealth(serviceCfg.Name, targetCfg.URL, true)

				hc.wg.Add(1)
				go hc.monitorTarget(status)

			}

		} else {

			logging.Debugf("HealthChecker: No health checks configured or no targets for service %s", serviceCfg.Name)

		}
	}
}

// Stop signals all health checking routines to terminate and waits for them.
func (hc *HealthChecker) Stop() {

	logging.Info("HealthChecker: Stopping health checks...")

	close(hc.stopChan)
	hc.wg.Wait()

	logging.Info("HealthChecker: All health checks stopped.")

}

func (hc *HealthChecker) monitorTarget(status *targetStatus) {

	defer hc.wg.Done()
	logging.Infof("HealthChecker: Starting monitoring for target %s (service: %s)", status.targetURL, status.serviceName)

	// Use the specific interval from the health check config for this service
	interval := time.Duration(status.config.Interval) * time.Second
	if interval <= 0 {
		logging.Warnf("HealthChecker: Invalid health check interval for %s (service: %s), defaulting to 30s", status.targetURL, status.serviceName)
		interval = 30 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Perform an initial check immediately
	hc.performCheck(status)

	for {

		select {

		case <-ticker.C:
			hc.performCheck(status)

		case <-hc.stopChan:
			logging.Infof("HealthChecker: Stopping monitoring for target %s (service: %s)", status.targetURL, status.serviceName)
			return

		}

	}

}

func (hc *HealthChecker) performCheck(status *targetStatus) {

	status.mu.Lock()
	defer status.mu.Unlock()

	// Prefer readiness probe if defined, otherwise liveness
	checkPath := status.config.Ready
	probeType := "readiness"
	if checkPath == "" {
		checkPath = status.config.Live
		probeType = "liveness"
	}

	if checkPath == "" {
		logging.Debugf("HealthChecker: No ready or live probe defined for target %s (service: %s)", status.targetURL, status.serviceName)
		return // No endpoint to check
	}

	// Ensure checkPath starts with a slash if it doesn't have a scheme/host
	if !strings.HasPrefix(checkPath, "http://") && !strings.HasPrefix(checkPath, "https://") && !strings.HasPrefix(checkPath, "/") {
		checkPath = "/" + checkPath
	}

	// Construct the full health check URL
	baseTargetURL, err := url.Parse(status.targetURL)
	if err != nil {
		logging.Errorf("HealthChecker: Failed to parse base target URL %s for service %s: %v", status.targetURL, status.serviceName, err)
		return
	}

	var fullCheckURL string
	if strings.HasPrefix(checkPath, "http://") || strings.HasPrefix(checkPath, "https://") {

		// If checkPath is an absolute URL, use it directly
		fullCheckURL = checkPath

	} else {

		// If checkPath is relative, resolve it against the base target URL
		healthURL := &url.URL{Path: checkPath}
		fullCheckURL = baseTargetURL.ResolveReference(healthURL).String()

	}

	timeout := time.Duration(status.config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Second // Default if not set or invalid
	}

	client := &http.Client{Timeout: timeout}
	req, err := http.NewRequest("GET", fullCheckURL, nil)

	if err != nil {
		logging.Errorf("HealthChecker: Failed to create health check request for %s (%s probe at %s): %v", status.targetURL, probeType, fullCheckURL, err)
		hc.processCheckResult(status, false) // Treat request creation failure as a check failure
		return
	}

	// Add any specific headers if needed, e.g., User-Agent
	req.Header.Set("User-Agent", "Corridor-HealthChecker/1.0")

	logging.Debugf("HealthChecker: Pinging %s probe for %s at %s (timeout: %v)", probeType, status.targetURL, fullCheckURL, timeout)
	resp, err := client.Do(req)

	if err != nil {

		logging.Warnf("HealthChecker: Health check failed for %s (%s probe at %s): %v", status.targetURL, probeType, fullCheckURL, err)
		hc.processCheckResult(status, false)
		return
	}
	defer resp.Body.Close()

	// Typically, a 2xx status code indicates health.
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {

		logging.Debugf("HealthChecker: Health check successful for %s (%s probe at %s), status: %d", status.targetURL, probeType, fullCheckURL, resp.StatusCode)
		hc.processCheckResult(status, true)

	} else {

		logging.Warnf("HealthChecker: Health check unsuccessful for %s (%s probe at %s), status: %d", status.targetURL, probeType, fullCheckURL, resp.StatusCode)
		hc.processCheckResult(status, false)

	}
}

func (hc *HealthChecker) processCheckResult(status *targetStatus, success bool) {

	previouslyHealthy := status.isHealthy

	if success {

		status.consecutiveSuccesses++
		status.consecutiveFailures = 0
		if !status.isHealthy && status.consecutiveSuccesses >= status.config.HealthyThreshold {
			status.isHealthy = true
			logging.Infof("HealthChecker: Target %s (service: %s) is now HEALTHY after %d successful checks.", status.targetURL, status.serviceName, status.consecutiveSuccesses)
		}

	} else {

		status.consecutiveFailures++
		status.consecutiveSuccesses = 0
		if status.isHealthy && status.consecutiveFailures >= status.config.UnhealthyThreshold {
			status.isHealthy = false
			logging.Warnf("HealthChecker: Target %s (service: %s) is now UNHEALTHY after %d failed checks.", status.targetURL, status.serviceName, status.consecutiveFailures)
		}

	}

	if status.isHealthy != previouslyHealthy {
		hc.proxy.UpdateTargetHealth(status.serviceName, status.targetURL, status.isHealthy)
	}
}
