package plugins

import (
	"net/http"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
	"github.com/google/uuid"
)

const (
	requestIDHeader     = "X-Request-ID"
	requestIDPluginName = "request-id"
)

// RequestIDPlugin ensures each request has an X-Request-ID header.
type RequestIDPlugin struct {
	// This plugin is simple and doesn't require specific configuration,
	// but we could add fields here if it did.
	// Example: overwriteExisting bool
}

// init registers the RequestIDPlugin with the plugin registry.
func init() {
	RegisterPlugin(requestIDPluginName, func() Plugin {
		return &RequestIDPlugin{}
	})
}

// Name returns the name of the plugin.
func (p *RequestIDPlugin) Name() string {
	return requestIDPluginName
}

// Init initializes the plugin. For RequestIDPlugin, there's no specific config needed.
func (p *RequestIDPlugin) Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error {

	logging.Debugf("Plugin [%s]: Initializing...", p.Name())
	// Example of how you might read config:
	// if pluginConfig != nil {
	//     if val, ok := pluginConfig["overwriteExisting"].(bool); ok {
	//         p.overwriteExisting = val
	//     }
	// }
	// For this plugin, no specific initialization is required beyond what's in the struct.
	return nil

}

// Handle is the middleware function for the RequestIDPlugin.
// It checks for an X-Request-ID header and adds one if it's missing.
func (p *RequestIDPlugin) Handle(next http.Handler) http.Handler {

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

		requestID := r.Header.Get(requestIDHeader)

		if requestID == "" {
			requestID = uuid.New().String()
			r.Header.Set(requestIDHeader, requestID)
			logging.Debugf("Plugin [%s]: No %s found, generated new one: %s", p.Name(), requestIDHeader, requestID)

		} else {

			logging.Debugf("Plugin [%s]: Found existing %s: %s", p.Name(), requestIDHeader, requestID)

		}

		// Also set it on the response headers for client visibility/tracing
		w.Header().Set(requestIDHeader, requestID)

		next.ServeHTTP(w, r)

	})

}
