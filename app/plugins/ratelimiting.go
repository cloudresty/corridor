package plugins

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

const (
	rateLimitingPluginName = "rate-limiting"
	defaultRateLimit       = 60 // Default requests per minute if not specified
	defaultRateWindow      = time.Minute
)

// clientRequestLog stores timestamps of requests for a client.
type clientRequestLog struct {
	timestamps []time.Time
	mu         sync.Mutex
}

// RateLimitingPlugin enforces request rate limits.
type RateLimitingPlugin struct {
	requestsPerWindow int
	window            time.Duration
	strategy          string // e.g., "ip-address"
	// In-memory store for rate limiting. Key: client identifier (e.g., IP address)
	// For a distributed gateway, a distributed store like Redis would be needed.
	clientStore map[string]*clientRequestLog
	storeMu     sync.RWMutex // Mutex for the clientStore map itself
}

// init registers the RateLimitingPlugin.
func init() {
	RegisterPlugin(rateLimitingPluginName, func() Plugin {
		return &RateLimitingPlugin{
			clientStore: make(map[string]*clientRequestLog),
		}
	})
}

// Name returns the plugin's name.
func (p *RateLimitingPlugin) Name() string {
	return rateLimitingPluginName
}

// Init initializes the RateLimitingPlugin with its configuration.
func (p *RateLimitingPlugin) Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error {

	logging.Debugf("Plugin [%s]: Initializing...", p.Name())

	// Set defaults
	p.requestsPerWindow = defaultRateLimit
	p.window = defaultRateWindow
	p.strategy = "ip-address" // Default strategy

	// Apply global config
	if globalPluginsConfig != nil && globalPluginsConfig.RateLimiting != nil {

		if strategy, ok := globalPluginsConfig.RateLimiting["strategy"].(string); ok && strategy != "" {
			p.strategy = strings.ToLower(strategy)
			logging.Debugf("Plugin [%s]: Set strategy to '%s' from global config", p.Name(), p.strategy)
		}
		// Could also have global default limits here

	}

	// Apply route-specific config (overrides global)
	if pluginConfig != nil {

		if rpm, ok := pluginConfig["requests_per_minute"].(int); ok && rpm > 0 {

			p.requestsPerWindow = rpm
			p.window = time.Minute // Assuming "requests_per_minute" implies a 1-minute window
			logging.Debugf("Plugin [%s]: Set rate limit to %d requests per minute from route config", p.Name(), rpm)

		} else if rps, ok := pluginConfig["requests_per_second"].(int); ok && rps > 0 {

			p.requestsPerWindow = rps
			p.window = time.Second
			logging.Debugf("Plugin [%s]: Set rate limit to %d requests per second from route config", p.Name(), rps)

		}
		// Could add more granular configurations like "requests_per_hour", etc.
	}

	if p.strategy != "ip-address" {

		logging.Warnf("Plugin [%s]: Currently only 'ip-address' strategy is fully implemented. Configured: '%s'", p.Name(), p.strategy)
		// For now, we'll proceed with ip-address if another is configured but not implemented.
		// Or we could return an error:
		// return fmt.Errorf("unsupported rate limiting strategy: %s", p.strategy)

	}

	logging.Infof("Plugin [%s]: Configured with %d requests per %v, strategy: '%s'", p.Name(), p.requestsPerWindow, p.window, p.strategy)

	// Start a goroutine to periodically clean up old entries from clientStore
	// to prevent memory leaks. This is a simple cleanup.
	// More sophisticated cleanup might be needed for very high cardinality of clients.
	go p.cleanupOldClients()

	return nil
}

// Handle is the middleware function for the RateLimitingPlugin.
func (p *RateLimitingPlugin) Handle(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		var clientID string

		switch p.strategy {

		case "ip-address":
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				// If we can't get the IP, we might deny the request or use RemoteAddr as is.
				// Using RemoteAddr directly might include port, leading to different client IDs for same IP.
				logging.Warnf("Plugin [%s]: Could not parse IP from RemoteAddr '%s': %v. Denying request.", p.Name(), r.RemoteAddr, err)
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			clientID = ip

		default:
			// Fallback or error for unsupported strategies
			logging.Errorf("Plugin [%s]: Unsupported rate limiting strategy '%s'. Denying request.", p.Name(), p.strategy)
			http.Error(w, "Internal Server Error: Rate limiting misconfiguration", http.StatusInternalServerError)
			return
		}

		if clientID == "" {

			logging.Errorf("Plugin [%s]: Client identifier for rate limiting is empty. Denying request.", p.Name())
			http.Error(w, "Forbidden", http.StatusForbidden)
			return

		}

		// Get or create the request log for this client
		p.storeMu.RLock()
		log, exists := p.clientStore[clientID]
		p.storeMu.RUnlock()
		logging.Debugf("Plugin [%s]: Client %s, store entry exists: %v", p.Name(), clientID, exists) // Added Debug Log

		if !exists {

			p.storeMu.Lock()
			// Double-check after acquiring write lock
			if log, exists = p.clientStore[clientID]; !exists {
				log = &clientRequestLog{}
				p.clientStore[clientID] = log
			}
			p.storeMu.Unlock()

		}

		// Perform rate limiting check
		log.mu.Lock()
		now := time.Now()

		logging.Debugf("Plugin [%s]: Client %s (at start), log.timestamps: %v", p.Name(), clientID, log.timestamps) // Log #1

		// Remove timestamps older than the current window
		validTimestamps := make([]time.Time, 0, len(log.timestamps))

		for _, ts := range log.timestamps {

			if now.Sub(ts) <= p.window {
				validTimestamps = append(validTimestamps, ts)
			} else {
				logging.Debugf("Plugin [%s]: Client %s pruning old timestamp %v (now %v)", p.Name(), clientID, ts, now) // Log #2
			}

		}
		log.timestamps = validTimestamps                                                                                 // Update with only the valid ones
		logging.Debugf("Plugin [%s]: Client %s (after pruning), log.timestamps: %v", p.Name(), clientID, log.timestamps) // Log #3

		// Check if the current number of requests IN THE WINDOW already meets or exceeds the limit
		// BEFORE adding the current one.
		if len(log.timestamps) >= p.requestsPerWindow {

			log.mu.Unlock()
			logging.Warnf("Plugin [%s]: Rate limit exceeded for client %s. Requests: %d/%d per %v",
				p.Name(), clientID, len(log.timestamps), p.requestsPerWindow, p.window)

			logging.Debugf("Plugin [%s]: Client %s, REJECTING request due to limit.", p.Name(), clientID) // Log #4 (Reject)
			// Set standard rate limiting headers
			w.Header().Set("X-RateLimit-Limit", strconv.Itoa(p.requestsPerWindow))
			w.Header().Set("X-RateLimit-Remaining", "0")
			// Retry-After could be calculated based on when the oldest request in the window expires (not implemented)
			// For simplicity, let's set it to the window duration for now.
			w.Header().Set("Retry-After", strconv.Itoa(int(p.window.Seconds())))
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)

			return

		}

		// If len(log.timestamps) == p.requestsPerWindow, this request is the one that hits the limit exactly.
		// It should be allowed, but the next one (if it arrives within the window of the oldest current request)
		// will push the count to p.requestsPerWindow + 1 and then be blocked by the >= check above.

		// If we are here, the request is allowed. Now add its timestamp.
		log.timestamps = append(log.timestamps, now)
		log.mu.Unlock()

		logging.Debugf("Plugin [%s]: Client %s request allowed. Requests in window: %d/%d. Remaining: %d",
			p.Name(), clientID, len(log.timestamps), p.requestsPerWindow, p.requestsPerWindow-len(log.timestamps)) // Correct remaining calculation

		// Set standard rate limiting headers on success as well
		w.Header().Set("X-RateLimit-Limit", strconv.Itoa(p.requestsPerWindow))                         // Also log what limit we used
		w.Header().Set("X-RateLimit-Remaining", strconv.Itoa(p.requestsPerWindow-len(log.timestamps))) // Correct remaining calculation

		next.ServeHTTP(w, r)
	})
}

// cleanupOldClients periodically iterates through the clientStore and removes entries
// for clients that haven't made requests in a while (e.g., 5x the rate limit window).
func (p *RateLimitingPlugin) cleanupOldClients() {

	cleanupInterval := 5 * p.window
	if cleanupInterval < 5*time.Minute {
		cleanupInterval = 5 * time.Minute
	}
	expiryDuration := 10 * p.window

	ticker := time.NewTicker(cleanupInterval)
	defer ticker.Stop()

	for range ticker.C {

		p.storeMu.Lock()
		now := time.Now()
		cleanedCount := 0

		for clientID, log := range p.clientStore {

			log.mu.Lock()

			if len(log.timestamps) == 0 {
				log.mu.Unlock()
				continue
			}

			if now.Sub(log.timestamps[len(log.timestamps)-1]) > expiryDuration {
				delete(p.clientStore, clientID)
				cleanedCount++
			}

			log.mu.Unlock()
		}

		p.storeMu.Unlock()
		if cleanedCount > 0 {
			logging.Debugf("Plugin [%s]: Cleaned up %d inactive client entries from rate limit store.", p.Name(), cleanedCount)
		}
	}
}
