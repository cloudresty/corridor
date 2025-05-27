package plugins

import (
	"fmt"
	"net/http"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

// Plugin is the interface that all gateway plugins must implement.
// The Handle method allows a plugin to process the request, potentially
// short-circuiting the request chain or modifying the request/response.
// It returns an http.Handler that will be called next in the chain.
// If a plugin short-circuits (e.g., authentication failure), it should write
// the response itself and return nil or a handler that does nothing further.
type Plugin interface {
	Name() string
	Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error // Initialize with its specific config
	Handle(next http.Handler) http.Handler                                             // The core middleware logic
}

// Registry for storing plugin constructors.
var pluginRegistry = make(map[string]func() Plugin)

// RegisterPlugin allows plugins to register themselves.
// This function is typically called from the init() function of each plugin package.
func RegisterPlugin(name string, constructor func() Plugin) {

	if _, exists := pluginRegistry[name]; exists {
		// Allow re-registration for development/testing, but log it.
		// For production, you might want to panic or return an error.
		logging.Warnf("Plugin: Re-registering plugin with name '%s'", name)
	}

	pluginRegistry[name] = constructor
	logging.Debugf("Plugin: Registered plugin '%s'", name)

}

// GetPluginInstance creates and initializes a new instance of a registered plugin.
// It uses the plugin-specific configuration from a route and the global plugin defaults.
func GetPluginInstance(instanceConfig *config.PluginInstanceConfig, globalPluginsConfig *config.PluginsConfig) (Plugin, error) {

	constructor, exists := pluginRegistry[instanceConfig.Name]
	if !exists {
		return nil, fmt.Errorf("plugin '%s' not registered", instanceConfig.Name)
	}

	plugin := constructor()

	// Pass the specific instance config and the global plugins config for initialization
	// The plugin's Init method can decide how to merge/use these.
	err := plugin.Init(instanceConfig.Config, globalPluginsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize plugin '%s': %w", instanceConfig.Name, err)
	}

	logging.Debugf("Plugin: Initialized instance of plugin '%s'", instanceConfig.Name)
	return plugin, nil

}

// ChainPlugins takes a list of plugin instances and an ultimate handler (e.g., the proxy)
// and chains them together in the order they appear in the list.
// The first plugin in the config list wraps the second, which wraps the third, and so on,
// with the final plugin wrapping the ultimateHandler.
func ChainPlugins(plugins []Plugin, ultimateHandler http.Handler) http.Handler {

	if len(plugins) == 0 {
		return ultimateHandler
	}

	currentHandler := ultimateHandler

	for i := len(plugins) - 1; i >= 0; i-- {
		plugin := plugins[i]
		currentHandler = plugin.Handle(currentHandler)
		logging.Debugf("Plugin: Chained plugin '%s'", plugin.Name())
	}

	return currentHandler

}
