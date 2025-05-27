package plugins

import (
	"fmt"
	"net/http"
	"regexp"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

const pathRewritePluginName = "path-rewrite"

// PathRewritePlugin modifies the request path based on a regular expression and replacement pattern.
type PathRewritePlugin struct {
	regex       *regexp.Regexp
	replacement string
}

// init registers the PathRewritePlugin.
func init() {
	RegisterPlugin(pathRewritePluginName, func() Plugin {
		return &PathRewritePlugin{}
	})
}

// Name returns the plugin's name.
func (p *PathRewritePlugin) Name() string {
	return pathRewritePluginName
}

// Init initializes the PathRewritePlugin with its configuration.
func (p *PathRewritePlugin) Init(pluginConfig map[string]any, globalPluginsConfig *config.PluginsConfig) error {
	logging.Debugf("Plugin [%s]: Initializing...", p.Name())

	if pluginConfig == nil {
		return fmt.Errorf("plugin [%s]: missing configuration", p.Name())
	}

	regexStr, ok := pluginConfig["regex"].(string)
	if !ok || regexStr == "" {
		return fmt.Errorf("plugin [%s]: missing or invalid 'regex' in configuration", p.Name())
	}

	var err error
	p.regex, err = regexp.Compile(regexStr)
	if err != nil {
		return fmt.Errorf("plugin [%s]: invalid regular expression '%s': %w", p.Name(), regexStr, err)
	}

	p.replacement, ok = pluginConfig["replacement"].(string)
	if !ok {
		return fmt.Errorf("plugin [%s]: missing 'replacement' in configuration", p.Name())
	}

	logging.Infof("Plugin [%s]: Configured with regex '%s' and replacement '%s'", p.Name(), regexStr, p.replacement)
	return nil
}

// Handle is the middleware function for the PathRewritePlugin.
func (p *PathRewritePlugin) Handle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originalPath := r.URL.Path
		newPath := p.regex.ReplaceAllString(originalPath, p.replacement)

		if newPath != originalPath {
			r.URL.Path = newPath
			logging.Infof("Plugin [%s]: Rewrote path from '%s' to '%s'", p.Name(), originalPath, newPath)
		} else {
			logging.Debugf("Plugin [%s]: Path '%s' did not match regex, no rewrite applied", p.Name(), originalPath)
		}

		next.ServeHTTP(w, r)
	})
}
