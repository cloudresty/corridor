package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
	"github.com/cloudresty/corridor/router" // To get MatchedRouteInfo
)

// Proxy is the main reverse proxy handler.
type Proxy struct {
	globalConfig *config.GlobalConfig
	serviceLBs   map[string]LoadBalancer // Map service name to its load balancer
	httpClient   *http.Client            // Client used by the reverse proxy
	mu           sync.RWMutex            // To protect serviceLBs if we add dynamic updates
	transport    http.RoundTripper
}

// NewProxy creates a new Proxy instance.
func NewProxy(cfg *config.Config) (*Proxy, error) {

	// Determine strictness for service initialization
	failStrictly := false // Default to lenient behavior
	if cfg.Global.FailOnServiceInitError != nil {
		failStrictly = *cfg.Global.FailOnServiceInitError
	}

	serviceLBs := make(map[string]LoadBalancer)

	for i := range cfg.Services {

		serviceCfg := &cfg.Services[i] // Important to take address for loop variable
		if serviceCfg.LoadBalancing == nil || len(serviceCfg.LoadBalancing.Targets) == 0 {
			logging.Warnf("Proxy: Service %s has no load balancing targets defined in config. It will not be routable.", serviceCfg.Name)
			continue
		}

		lb, err := NewLoadBalancer(serviceCfg)
		if err != nil {
			logging.Errorf("Proxy: Failed to create load balancer for service %s: %v", serviceCfg.Name, err)
			if failStrictly {
				return nil, fmt.Errorf("failed to create load balancer for service %s (strict mode enabled): %w", serviceCfg.Name, err)
			}
			continue // Lenient mode: log and skip this service
		}

		serviceLBs[serviceCfg.Name] = lb
		logging.Infof("Proxy: Initialized load balancer for service %s with strategy %s", serviceCfg.Name, serviceCfg.LoadBalancing.Strategy)

	}

	// --- Configure HTTP Transport ---
	// Default values (can be overridden by config)
	dialTimeout := 5 * time.Second
	keepAlive := 30 * time.Second
	maxIdleConns := 100
	maxIdleConnsPerHost := 10
	idleConnTimeout := 90 * time.Second
	tlsHandshakeTimeout := 10 * time.Second
	expectContinueTimeout := 1 * time.Second
	responseHeaderTimeout := cfg.Global.GetDefaultTimeout() // Default to global request timeout
	if responseHeaderTimeout <= 0 {
		responseHeaderTimeout = 30 * time.Second // Fallback if global default is also not set
	}

	// Override with configured values if present
	if cfg.Global.TransportDialTimeoutSec != nil && *cfg.Global.TransportDialTimeoutSec > 0 {
		dialTimeout = time.Duration(*cfg.Global.TransportDialTimeoutSec) * time.Second
	}
	if cfg.Global.TransportKeepAliveSec != nil && *cfg.Global.TransportKeepAliveSec > 0 {
		keepAlive = time.Duration(*cfg.Global.TransportKeepAliveSec) * time.Second
	}
	if cfg.Global.TransportMaxIdleConns != nil && *cfg.Global.TransportMaxIdleConns > 0 {
		maxIdleConns = *cfg.Global.TransportMaxIdleConns
	}
	if cfg.Global.TransportMaxIdleConnsPerHost != nil && *cfg.Global.TransportMaxIdleConnsPerHost > 0 {
		maxIdleConnsPerHost = *cfg.Global.TransportMaxIdleConnsPerHost
	}
	if cfg.Global.TransportIdleConnTimeoutSec != nil && *cfg.Global.TransportIdleConnTimeoutSec > 0 {
		idleConnTimeout = time.Duration(*cfg.Global.TransportIdleConnTimeoutSec) * time.Second
	}
	if cfg.Global.TransportTLSHandshakeTimeoutSec != nil && *cfg.Global.TransportTLSHandshakeTimeoutSec > 0 {
		tlsHandshakeTimeout = time.Duration(*cfg.Global.TransportTLSHandshakeTimeoutSec) * time.Second
	}
	if cfg.Global.TransportExpectContinueTimeoutSec != nil && *cfg.Global.TransportExpectContinueTimeoutSec > 0 {
		expectContinueTimeout = time.Duration(*cfg.Global.TransportExpectContinueTimeoutSec) * time.Second
	}
	if cfg.Global.TransportResponseHeaderTimeoutSec != nil && *cfg.Global.TransportResponseHeaderTimeoutSec > 0 {
		responseHeaderTimeout = time.Duration(*cfg.Global.TransportResponseHeaderTimeoutSec) * time.Second
	} else if responseHeaderTimeout <= 0 { // Ensure a positive fallback if default_timeout_ms was also 0
		responseHeaderTimeout = 30 * time.Second
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, // Respect proxy environment variables
		DialContext: (&net.Dialer{ // Use net.Dialer for more control
			Timeout:   dialTimeout,
			KeepAlive: keepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true, // Prefer HTTP/2
		MaxIdleConns:          maxIdleConns,
		MaxIdleConnsPerHost:   maxIdleConnsPerHost,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ExpectContinueTimeout: expectContinueTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		// WriteBufferSize and ReadBufferSize can be tuned for performance
	}

	return &Proxy{
		globalConfig: &cfg.Global,
		serviceLBs:   serviceLBs,
		transport:    transport,
		// httpClient will be implicitly created by ReverseProxy if not set,
		// but we can create one if we need more control or want to reuse it.
		// For ReverseProxy, setting its Transport is key.
	}, nil
}

// ServeHTTP handles the incoming request, selects an upstream, and proxies.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	// 1. Get MatchedRouteInfo from context (set by the router)
	routeInfo, ok := r.Context().Value(router.MatchedRouteKey).(*router.MatchedRouteInfo)
	if !ok || routeInfo == nil || routeInfo.ServiceConfig == nil {
		logging.Errorw("Proxy: MatchedRouteInfo not found or incomplete in request context.", map[string]any{"path": r.URL.Path})
		http.Error(w, "Internal Server Error: Route information missing", http.StatusInternalServerError)
		return
	}

	serviceName := routeInfo.ServiceConfig.Name
	logging.Debugf("Proxy: Handling request for service %s (route ID: %s)", serviceName, routeInfo.RouteConfig.ID)

	// 2. Get LoadBalancer for the service
	p.mu.RLock()
	lb, serviceExists := p.serviceLBs[serviceName]
	p.mu.RUnlock()

	if !serviceExists {
		logging.Errorw("Proxy: No load balancer found for service.", map[string]any{"service": serviceName, "path": r.URL.Path})
		http.Error(w, fmt.Sprintf("Service '%s' not configured or unavailable", serviceName), http.StatusBadGateway)
		return
	}

	// 3. Select an upstream target using the LoadBalancer
	upstreamTarget, err := lb.Next()
	if err != nil {
		logging.Errorw("Proxy: Failed to get next upstream target.", map[string]any{
			"service": serviceName,
			"error":   err.Error(),
			"path":    r.URL.Path,
		})
		http.Error(w, fmt.Sprintf("No healthy upstream available for service '%s'", serviceName), http.StatusServiceUnavailable)
		return
	}

	// For least-connections: Increment active connections *before* sending.
	// The decrement will happen via the custom round tripper.
	// This needs to be done carefully if the load balancer strategy is "least-connections".
	// We'll add this logic after creating the reverseProxy instance.
	// upstreamTarget.IncrementActiveConnections() // This will be done before ServeHTTP

	targetURL := upstreamTarget.URL
	logging.Infof("Proxy: Forwarding request for %s to upstream %s (service: %s)", r.URL.Path, targetURL.String(), serviceName)

	// 4. Create and configure the ReverseProxy
	reverseProxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			// Modify the request to point to the chosen upstream
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = singleJoiningSlash(targetURL.Path, req.URL.Path) // Preserve original request path relative to upstream base
			if targetURL.RawQuery == "" || req.URL.RawQuery == "" {
				req.URL.RawQuery = targetURL.RawQuery + req.URL.RawQuery
			} else {
				req.URL.RawQuery = targetURL.RawQuery + "&" + req.URL.RawQuery
			}

			// Set X-Forwarded-Host or use original Host
			if _, ok := req.Header["User-Agent"]; !ok {
				// explicitly disable User-Agent so it's not set to default value
				req.Header.Set("User-Agent", "")
			}
			req.Host = targetURL.Host // Set the Host header to the upstream's host

			// Add configured proxy headers
			for _, headerName := range p.globalConfig.ProxyHeaders {

				switch strings.ToLower(headerName) {

				case "x-request-id":
					// If X-Request-ID is already set by client or previous middleware, preserve it.
					// Otherwise, generate one (TODO: implement request ID generation if needed).
					if req.Header.Get(headerName) == "" {
						// For now, let's not generate one here, assume it might come from a plugin
						// req.Header.Set(headerName, "generated-id")
					}

				case "x-forwarded-for":
					// httputil.ReverseProxy adds X-Forwarded-For by default if not already present.
					// If it's present, it appends. This is usually desired.
					// We can also explicitly manage it if needed.
					// Example: clientIP, _ := net.SplitHostPort(r.RemoteAddr)
					// req.Header.Add(headerName, clientIP)
					break // Default behavior is good

				case "x-forwarded-proto":
					req.Header.Set(headerName, r.URL.Scheme)

				case "x-forwarded-host":
					req.Header.Set(headerName, r.Host)

				default:
					// For other custom headers, copy from original request if they exist,
					// or set them if they have specific meaning for the proxy.
					// This part needs careful consideration based on desired behavior.
					// For now, we assume these are headers to be *added* or *overwritten*.
					// If the intent is to pass through client headers, that's a different logic.
					// The current config implies these are headers the proxy itself should manage/add.
				}

			}

		},

		// Transport: p.transport, // We will wrap this transport
		Transport: &countingRoundTripper{
			Proxied: p.transport,    // Our configured base transport
			Target:  upstreamTarget, // The specific target for this request
		},

		ErrorHandler: func(rw http.ResponseWriter, req *http.Request, err error) {
			logging.Errorw("Proxy: Upstream request error.", map[string]interface{}{
				"service":  serviceName,
				"upstream": targetURL.String(),
				"path":     req.URL.Path,
				"error":    err.Error(),
			})
			// Mark target as unhealthy? This needs coordination with health checker.
			// Decrementing connections is handled by countingRoundTripper's error path.
			rw.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(rw, "Error connecting to upstream service: %s", serviceName)
		},

		ModifyResponse: func(resp *http.Response) error {
			// Opportunity to modify the response from the upstream before sending to client
			logging.Debugf("Proxy: Received response from upstream %s for %s with status %d",
				targetURL.String(), resp.Request.URL.Path, resp.StatusCode)
			return nil
		},
		// BufferPool can be used for performance optimization
	}

	// Set a timeout for the entire proxy operation if needed, using context
	// The default_timeout_ms from global config can be used here.
	ctx := r.Context()
	var cancel context.CancelFunc
	if p.globalConfig.DefaultTimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(r.Context(), p.globalConfig.GetDefaultTimeout())
		defer cancel()
	}

	// Increment active connections for the chosen target *before* proxying
	upstreamTarget.IncrementActiveConnections()
	// Decrement will be handled by countingRoundTripper
	reverseProxy.ServeHTTP(w, r.WithContext(ctx))
	// Note: Decrementing here directly after ServeHTTP might be too early if ServeHTTP is asynchronous
	// or if the request body is streamed. The RoundTripper approach is more robust.
}

// singleJoiningSlash joins two URL path components with a single slash.
// From httputil.NewSingleHostReverseProxy
func singleJoiningSlash(a, b string) string {

	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")

	switch {

	case aslash && bslash:
		return a + b[1:]

	case !aslash && !bslash:
		if b == "" { // if b is empty, don't add a slash
			return a
		}
		return a + "/" + b
	}

	return a + b

}

// GetLoadBalancer allows external access to a service's load balancer, e.g., for health checker.
func (p *Proxy) GetLoadBalancer(serviceName string) (LoadBalancer, bool) {

	p.mu.RLock()
	defer p.mu.RUnlock()

	lb, ok := p.serviceLBs[serviceName]
	return lb, ok

}

// UpdateTargetHealth is a method that could be called by the health checker
// to update the health status of a specific target within a service's load balancer.
// This is a simplified example; a more robust system might involve channels or callbacks.
func (p *Proxy) UpdateTargetHealth(serviceName string, targetURL string, healthy bool) {

	p.mu.RLock() // RLock for reading serviceLBs map
	lb, serviceExists := p.serviceLBs[serviceName]
	p.mu.RUnlock()

	if !serviceExists {
		logging.Warnf("Proxy: Attempted to update health for unknown service %s", serviceName)
		return
	}

	// The LoadBalancer interface would need a method to find and update a specific target.
	// For now, let's assume the LoadBalancer implementation handles this internally
	// or we iterate its targets. This is a bit of a simplification.
	// A better approach: lb.UpdateTargetHealth(targetURL, healthy) if LB interface supports it.

	// Temporary direct manipulation for demonstration (not ideal for interface abstraction)
	switch concreteLB := lb.(type) {

	case *roundRobinLoadBalancer:
		concreteLB.mu.Lock() // Lock specific LB for modification
		for _, t := range concreteLB.targets {
			if t.URL.String() == targetURL {
				t.SetHealth(healthy)
				logging.Infof("Proxy: Health status updated for target %s in service %s to %t", targetURL, serviceName, healthy)
				break
			}
		}
		concreteLB.mu.Unlock()

	case *randomLoadBalancer:
		concreteLB.mu.Lock()
		for _, t := range concreteLB.targets {
			if t.URL.String() == targetURL {
				t.SetHealth(healthy)
				logging.Infof("Proxy: Health status updated for target %s in service %s to %t", targetURL, serviceName, healthy)
				break
			}
		}
		concreteLB.mu.Unlock()

	default:
		logging.Warnf("Proxy: Cannot update target health for service %s, unknown load balancer type", serviceName)
	}

}

// countingRoundTripper wraps an http.RoundTripper to decrement active connections
// for a specific UpstreamTarget when the request is done.
type countingRoundTripper struct {
	Proxied http.RoundTripper
	Target  *UpstreamTarget
}

func (crt *countingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// The Target's active connections were already incremented before this RoundTrip call.
	// We must ensure DecrementActiveConnections is called regardless of success or failure.
	defer crt.Target.DecrementActiveConnections()

	res, err := crt.Proxied.RoundTrip(req)
	// Logging for connection decrement:
	// logging.Debugf("Proxy: Decremented active connections for target %s (service: %s). Current: %d",
	// 	crt.Target.URL.String(), crt.Target.GetServiceName(), crt.Target.GetActiveConnections())
	// GetServiceName() is not on UpstreamTarget, would need to pass service name or have target know it.
	return res, err
}
