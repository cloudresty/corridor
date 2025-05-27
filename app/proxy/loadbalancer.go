package proxy

import (
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

// UpstreamTarget represents a backend server instance.
type UpstreamTarget struct {
	URL     *url.URL
	Healthy bool
	Weight  int
	// activeConnections tracks the number of active requests being handled by this target.
	activeConnections int64        // Use atomic operations for this
	mu                sync.RWMutex // For thread-safe updates to Healthy status
}

// SetHealth updates the health status of the target.
func (ut *UpstreamTarget) SetHealth(healthy bool) {

	ut.mu.Lock()
	defer ut.mu.Unlock()
	ut.Healthy = healthy

}

// IsHealthy checks the health status of the target.
func (ut *UpstreamTarget) IsHealthy() bool {

	ut.mu.RLock()
	defer ut.mu.RUnlock()
	return ut.Healthy

}

// IncrementActiveConnections atomically increases the active connection count.
func (ut *UpstreamTarget) IncrementActiveConnections() {

	atomic.AddInt64(&ut.activeConnections, 1)

}

// DecrementActiveConnections atomically decreases the active connection count.
func (ut *UpstreamTarget) DecrementActiveConnections() {

	atomic.AddInt64(&ut.activeConnections, -1)

}

// GetActiveConnections atomically retrieves the active connection count.
func (ut *UpstreamTarget) GetActiveConnections() int64 {

	return atomic.LoadInt64(&ut.activeConnections)

}

// LoadBalancer defines the interface for selecting an upstream target.
type LoadBalancer interface {
	Next() (*UpstreamTarget, error)
	UpdateTargets([]*UpstreamTarget) // For dynamic updates if needed
	GetServiceName() string
	// NotifyConnectionState can be called by the proxy to update connection counts
	// This is one way to handle it; another is direct calls to Increment/Decrement on UpstreamTarget.
	// For simplicity, we'll have the proxy call Increment/Decrement directly on the chosen UpstreamTarget.

}

// NewLoadBalancer creates a new load balancer based on the service configuration.
func NewLoadBalancer(serviceCfg *config.ServiceConfig) (LoadBalancer, error) {

	if serviceCfg.LoadBalancing == nil || len(serviceCfg.LoadBalancing.Targets) == 0 {
		return nil, fmt.Errorf("service %s: load balancing configuration or targets are missing", serviceCfg.Name)
	}

	targets := make([]*UpstreamTarget, 0, len(serviceCfg.LoadBalancing.Targets))

	for _, t := range serviceCfg.LoadBalancing.Targets {

		parsedURL, err := url.Parse(t.URL)
		if err != nil {
			logging.Warnf("Service %s: Failed to parse upstream URL %s: %v. Skipping target.", serviceCfg.Name, t.URL, err)
			continue
		}

		targets = append(targets, &UpstreamTarget{
			URL:     parsedURL,
			Healthy: true, // Assume healthy initially; health checker will update
			Weight:  t.Weight,
		})

	}

	if len(targets) == 0 {

		// If all target URLs were invalid, serviceCfg.LoadBalancing.Targets might still be populated
		// but our `targets` slice (of *UpstreamTarget) would be empty.
		// This check ensures we have processable targets.
		return nil, fmt.Errorf("service %s: no valid upstream targets could be initialized", serviceCfg.Name)

	}

	strategy := strings.ToLower(serviceCfg.LoadBalancing.Strategy)
	logging.Debugf("Service %s: Initializing load balancer with strategy '%s' and %d targets", serviceCfg.Name, strategy, len(targets))

	switch strategy {

	case "round-robin":
		return newRoundRobinLoadBalancer(serviceCfg.Name, targets), nil

	case "random":
		return newRandomLoadBalancer(serviceCfg.Name, targets), nil

	case "weighted":

		// Ensure all targets have a weight if strategy is weighted
		for _, t := range targets {
			if t.Weight <= 0 {
				logging.Warnf("Service %s (weighted): Target %s has invalid weight %d. Defaulting to weight 1.", serviceCfg.Name, t.URL.String(), t.Weight)
				t.Weight = 1 // Default to 1 if weight is not positive
			}
		}
		return newWeightedRandomLoadBalancer(serviceCfg.Name, targets), nil

	case "least-connections":
		return newLeastConnectionsLoadBalancer(serviceCfg.Name, targets), nil

	default:
		logging.Warnf("Service %s: Unsupported load balancing strategy '%s', defaulting to round-robin.", serviceCfg.Name, strategy)
		return newRoundRobinLoadBalancer(serviceCfg.Name, targets), nil
	}

}

// --- RoundRobinLoadBalancer ---
type roundRobinLoadBalancer struct {
	serviceName string
	targets     []*UpstreamTarget
	nextIndex   uint32
	mu          sync.RWMutex
}

func newRoundRobinLoadBalancer(serviceName string, targets []*UpstreamTarget) *roundRobinLoadBalancer {

	return &roundRobinLoadBalancer{
		serviceName: serviceName,
		targets:     targets,
		nextIndex:   0,
	}

}

func (lb *roundRobinLoadBalancer) GetServiceName() string {
	return lb.serviceName
}

func (lb *roundRobinLoadBalancer) Next() (*UpstreamTarget, error) {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.targets) == 0 {
		return nil, fmt.Errorf("service %s (round-robin): no targets available", lb.serviceName)
	}

	// Simple round robin: try up to len(targets) times to find a healthy one
	numTargets := len(lb.targets)
	for range numTargets {
		currentIndex := atomic.AddUint32(&lb.nextIndex, 1) - 1
		target := lb.targets[currentIndex%uint32(numTargets)]
		if target.IsHealthy() {
			return target, nil
		}
		logging.Debugf("Service %s (round-robin): Target %s is unhealthy, trying next.", lb.serviceName, target.URL.String())
	}
	return nil, fmt.Errorf("service %s (round-robin): no healthy targets available after checking all", lb.serviceName)
}

func (lb *roundRobinLoadBalancer) UpdateTargets(targets []*UpstreamTarget) {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.targets = targets
	atomic.StoreUint32(&lb.nextIndex, 0) // Reset index on update
	logging.Infof("Service %s (round-robin): Targets updated. New count: %d", lb.serviceName, len(lb.targets))

}

// --- RandomLoadBalancer ---
type randomLoadBalancer struct {
	serviceName string
	targets     []*UpstreamTarget
	rand        *rand.Rand
	mu          sync.RWMutex
}

func newRandomLoadBalancer(serviceName string, targets []*UpstreamTarget) *randomLoadBalancer {

	return &randomLoadBalancer{
		serviceName: serviceName,
		targets:     targets,
		rand:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}

}

func (lb *randomLoadBalancer) GetServiceName() string {
	return lb.serviceName
}

func (lb *randomLoadBalancer) Next() (*UpstreamTarget, error) {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.targets) == 0 {
		return nil, fmt.Errorf("service %s (random): no targets available", lb.serviceName)
	}

	// Filter healthy targets
	healthyTargets := make([]*UpstreamTarget, 0, len(lb.targets))
	for _, t := range lb.targets {
		if t.IsHealthy() {
			healthyTargets = append(healthyTargets, t)
		}
	}

	if len(healthyTargets) == 0 {
		return nil, fmt.Errorf("service %s (random): no healthy targets available", lb.serviceName)
	}

	randomIndex := lb.rand.Intn(len(healthyTargets))
	return healthyTargets[randomIndex], nil
}

func (lb *randomLoadBalancer) UpdateTargets(targets []*UpstreamTarget) {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.targets = targets
	logging.Infof("Service %s (random): Targets updated. New count: %d", lb.serviceName, len(lb.targets))

}

// --- WeightedRandomLoadBalancer ---
type weightedRandomLoadBalancer struct {
	serviceName string
	targets     []*UpstreamTarget
	rand        *rand.Rand
	mu          sync.RWMutex
}

func newWeightedRandomLoadBalancer(serviceName string, targets []*UpstreamTarget) *weightedRandomLoadBalancer {

	return &weightedRandomLoadBalancer{
		serviceName: serviceName,
		targets:     targets,
		rand:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}

}

func (lb *weightedRandomLoadBalancer) GetServiceName() string {
	return lb.serviceName
}

func (lb *weightedRandomLoadBalancer) Next() (*UpstreamTarget, error) {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.targets) == 0 {
		return nil, fmt.Errorf("service %s (weighted-random): no targets available", lb.serviceName)
	}

	healthyTargets := make([]*UpstreamTarget, 0, len(lb.targets))
	totalWeight := 0
	for _, t := range lb.targets {
		if t.IsHealthy() {
			healthyTargets = append(healthyTargets, t)
			totalWeight += t.Weight
		}
	}

	if len(healthyTargets) == 0 {
		return nil, fmt.Errorf("service %s (weighted-random): no healthy targets available", lb.serviceName)
	}

	if totalWeight <= 0 {
		// This case should ideally not happen if weights are validated to be > 0.
		// Fallback to simple random among healthy targets if totalWeight is 0.
		logging.Warnf("Service %s (weighted-random): Total weight of healthy targets is %d. Falling back to simple random.", lb.serviceName, totalWeight)
		randomIndex := lb.rand.Intn(len(healthyTargets))
		return healthyTargets[randomIndex], nil
	}

	randomNumber := lb.rand.Intn(totalWeight) // A random number from 0 to totalWeight-1
	cumulativeWeight := 0
	for _, target := range healthyTargets {
		cumulativeWeight += target.Weight
		if randomNumber < cumulativeWeight {
			return target, nil
		}
	}

	// Should not be reached if totalWeight > 0 and healthyTargets is not empty.
	// Could indicate an issue with weight calculation or random number generation.
	// As a fallback, return the last healthy target.
	logging.Errorf("Service %s (weighted-random): Failed to select target, falling back to last healthy. This should not happen.", lb.serviceName)
	return healthyTargets[len(healthyTargets)-1], nil

}

func (lb *weightedRandomLoadBalancer) UpdateTargets(targets []*UpstreamTarget) {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.targets = targets
	logging.Infof("Service %s (weighted-random): Targets updated. New count: %d", lb.serviceName, len(lb.targets))

}

// --- LeastConnectionsLoadBalancer ---
type leastConnectionsLoadBalancer struct {
	serviceName string
	targets     []*UpstreamTarget
	// For breaking ties in least connections, we can use a round-robin approach on the subset of least-connected targets.
	// Or simply pick the first one found. For simplicity, we'll pick the first one.
	// A more advanced tie-breaking could use a random choice or round-robin index.
	mu sync.RWMutex
}

func newLeastConnectionsLoadBalancer(serviceName string, targets []*UpstreamTarget) *leastConnectionsLoadBalancer {

	return &leastConnectionsLoadBalancer{
		serviceName: serviceName,
		targets:     targets,
	}

}

func (lb *leastConnectionsLoadBalancer) GetServiceName() string {
	return lb.serviceName
}

func (lb *leastConnectionsLoadBalancer) Next() (*UpstreamTarget, error) {

	lb.mu.RLock()
	defer lb.mu.RUnlock()

	if len(lb.targets) == 0 {
		return nil, fmt.Errorf("service %s (least-connections): no targets available", lb.serviceName)
	}

	var bestTarget *UpstreamTarget
	minConnections := int64(-1) // Initialize with a value that will be overridden

	// Iterate through all targets to find healthy ones and then the one with the least connections.
	// If multiple targets have the same minimum number of connections, this will pick the first one encountered.
	for _, target := range lb.targets {
		if target.IsHealthy() {
			currentConns := target.GetActiveConnections()
			if bestTarget == nil || currentConns < minConnections {
				minConnections = currentConns
				bestTarget = target
			}
			// Simple tie-breaking: if connections are equal, we could introduce randomness or round-robin here.
			// For now, the first one encountered with minConnections wins.
		}
	}

	if bestTarget == nil {
		return nil, fmt.Errorf("service %s (least-connections): no healthy targets available", lb.serviceName)
	}

	// The chosen target's active connections will be incremented by the proxy *after* this selection.
	logging.Debugf("Service %s (least-connections): Selected target %s with %d active connections.", lb.serviceName, bestTarget.URL.String(), minConnections)
	return bestTarget, nil
}

func (lb *leastConnectionsLoadBalancer) UpdateTargets(targets []*UpstreamTarget) {

	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.targets = targets
	// Reset any tie-breaking counters if implemented
	logging.Infof("Service %s (least-connections): Targets updated. New count: %d", lb.serviceName, len(lb.targets))

}
