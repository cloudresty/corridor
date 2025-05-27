package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

const (
	cachingPluginName           = "caching"
	defaultCacheTTL             = 5 * time.Minute
	defaultCacheCleanupInterval = 10 * time.Minute
	maxCacheBodySize            = 1024 * 1024 // 1MB max body size to cache, to prevent OOM
)

// cacheEntry stores the cached response and its expiry time.
type cacheEntry struct {
	statusCode int
	header     http.Header
	body       []byte
	expiresAt  time.Time
}

// isExpired checks if the cache entry has expired.
func (ce *cacheEntry) isExpired() bool {
	return time.Now().After(ce.expiresAt)
}

// CachingPlugin provides response caching capabilities.
type CachingPlugin struct {
	ttl         time.Duration
	cacheStore  map[string]*cacheEntry
	storeMu     sync.RWMutex
	stopCleanup chan struct{}
}

// init registers the CachingPlugin.
func init() {
	RegisterPlugin(cachingPluginName, func() Plugin {
		return &CachingPlugin{
			cacheStore:  make(map[string]*cacheEntry),
			stopCleanup: make(chan struct{}),
		}
	})
}

// Name returns the plugin's name.
func (p *CachingPlugin) Name() string {
	return cachingPluginName
}

// Init initializes the CachingPlugin with its configuration.
func (p *CachingPlugin) Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error {

	logging.Debugf("Plugin [%s]: Initializing...", p.Name())
	p.ttl = defaultCacheTTL // Default TTL

	// Apply global default TTL
	if globalPluginsConfig != nil && globalPluginsConfig.Caching != nil {
		if defaultTTLSec, ok := globalPluginsConfig.Caching["default_ttl_seconds"].(int); ok && defaultTTLSec > 0 {
			p.ttl = time.Duration(defaultTTLSec) * time.Second
			logging.Debugf("Plugin [%s]: Set TTL to %v from global config", p.Name(), p.ttl)
		}
	}

	// Apply route-specific TTL (overrides global)
	if pluginConfig != nil {
		if ttlSec, ok := pluginConfig["ttl_seconds"].(int); ok && ttlSec > 0 {
			p.ttl = time.Duration(ttlSec) * time.Second
			logging.Debugf("Plugin [%s]: Set TTL to %v from route config", p.Name(), p.ttl)
		}
	}

	logging.Infof("Plugin [%s]: Configured with TTL %v", p.Name(), p.ttl)

	// Start a goroutine to periodically clean up expired cache entries.
	go p.cleanupExpiredEntries()

	return nil

}

// generateCacheKey creates a unique key for caching based on the request.
func (p *CachingPlugin) generateCacheKey(r *http.Request) string {
	// Key components: Method, Host, Full Path + Query.
	// Concatenate them and then hash for a robust key.
	rawKey := strings.ToUpper(r.Method) + ":" + r.Host + ":" + r.URL.RequestURI()

	hasher := sha256.New()
	hasher.Write([]byte(rawKey)) // Write the string as bytes to the hasher
	hashBytes := hasher.Sum(nil) // Get the resulting hash bytes

	return hex.EncodeToString(hashBytes) // Convert the hash bytes to a hex string
}

// Handle is the middleware function for the CachingPlugin.
func (p *CachingPlugin) Handle(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		// Only cache GET requests (typically)
		if r.Method != http.MethodGet {
			logging.Debugf("Plugin [%s]: Skipping cache for non-GET request: %s %s", p.Name(), r.Method, r.URL.Path)
			next.ServeHTTP(w, r)
			return
		}

		cacheKey := p.generateCacheKey(r)

		// 1. Check cache
		p.storeMu.RLock()
		entry, found := p.cacheStore[cacheKey]
		p.storeMu.RUnlock()

		if found && !entry.isExpired() {

			logging.Infof("Plugin [%s]: Cache HIT for key: %s", p.Name(), cacheKey)
			// Serve from cache
			for key, values := range entry.header {
				for _, value := range values {
					w.Header().Add(key, value)
				}
			}

			// Add a header to indicate response was served from cache (optional)
			w.Header().Set("X-Corridor-Cache-Status", "HIT")
			w.WriteHeader(entry.statusCode)
			if len(entry.body) > 0 {
				_, err := w.Write(entry.body)
				if err != nil {
					logging.Errorf("Plugin [%s]: Error writing cached response body: %v", p.Name(), err)
				}
			}

			return

		}

		if found && entry.isExpired() {

			logging.Infof("Plugin [%s]: Cache STALE for key: %s. Fetching from upstream.", p.Name(), cacheKey)
			// Optionally remove stale entry here, or let cleanup handle it
			p.storeMu.Lock()
			delete(p.cacheStore, cacheKey)
			p.storeMu.Unlock()

		} else {

			logging.Infof("Plugin [%s]: Cache MISS for key: %s. Fetching from upstream.", p.Name(), cacheKey)

		}

		w.Header().Set("X-Corridor-Cache-Status", "MISS") // Set MISS header before calling next

		// 2. If not in cache or expired, call next handler and cache the response
		// Use httptest.ResponseRecorder to capture the response from the next handler.
		recorder := httptest.NewRecorder()
		next.ServeHTTP(recorder, r)

		// Copy recorded response to the actual response writer
		for key, values := range recorder.Header() {

			// Don't copy our own cache status header from the recorder
			if key == "X-Corridor-Cache-Status" && w.Header().Get("X-Corridor-Cache-Status") == "MISS" {
				continue
			}

			for _, value := range values {
				w.Header().Add(key, value)
			}

		}

		w.WriteHeader(recorder.Code)

		responseBody := recorder.Body.Bytes() // Get the body before writing to w
		if len(responseBody) > 0 {

			_, err := w.Write(responseBody)
			if err != nil {
				logging.Errorf("Plugin [%s]: Error writing upstream response body: %v", p.Name(), err)
			}

		}

		// 3. Cache the response if it's cacheable (e.g., 200 OK) and body size is within limits.
		if recorder.Code == http.StatusOK {

			if len(responseBody) > maxCacheBodySize {
				logging.Warnf("Plugin [%s]: Response body for %s too large (%d bytes) to cache. Max: %d bytes.",
					p.Name(), cacheKey, len(responseBody), maxCacheBodySize)
				return
			}

			newEntry := &cacheEntry{
				statusCode: recorder.Code,
				header:     recorder.Header().Clone(), // Clone headers
				body:       responseBody,              // Already have the bytes
				expiresAt:  time.Now().Add(p.ttl),
			}

			p.storeMu.Lock()
			p.cacheStore[cacheKey] = newEntry
			p.storeMu.Unlock()
			logging.Infof("Plugin [%s]: Cached response for key: %s, TTL: %v", p.Name(), cacheKey, p.ttl)

		} else {

			logging.Debugf("Plugin [%s]: Response for key %s not cached due to status code: %d", p.Name(), cacheKey, recorder.Code)

		}

	})

}

// cleanupExpiredEntries periodically removes expired items from the cache.
func (p *CachingPlugin) cleanupExpiredEntries() {

	ticker := time.NewTicker(defaultCacheCleanupInterval)
	defer ticker.Stop()

	for {

		select {

		case <-ticker.C:
			p.storeMu.Lock() // Lock the entire cache for cleanup
			cleanedCount := 0
			for key, entry := range p.cacheStore {
				if entry.isExpired() {
					delete(p.cacheStore, key)
					cleanedCount++
				}
			}
			p.storeMu.Unlock()
			if cleanedCount > 0 {
				logging.Debugf("Plugin [%s]: Cleaned up %d expired cache entries.", p.Name(), cleanedCount)
			}

		case <-p.stopCleanup:
			logging.Debugf("Plugin [%s]: Stopping cache cleanup goroutine.", p.Name())
			return
		}

	}

}

// StopCleanup can be called if the plugin instance needs to be gracefully shut down.
// For simple plugins, this might not be strictly necessary if the program exits,
// but good practice for more complex plugins with persistent resources.
func (p *CachingPlugin) StopCleanup() {
	close(p.stopCleanup)
}
