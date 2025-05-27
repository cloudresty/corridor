package router

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
	"github.com/cloudresty/corridor/plugins"
)

// MatchedRouteInfo holds information about the matched route and service
type MatchedRouteInfo struct {
	RouteConfig   *config.RouteConfig
	ServiceConfig *config.ServiceConfig
}

// ContextKey is a type used for context keys to avoid collisions.
type ContextKey string

// MatchedRouteKey is the key used to store MatchedRouteInfo in the request context.
const MatchedRouteKey ContextKey = "matchedRouteInfo"

// Router is responsible for matching incoming requests to configured routes.
type Router struct {
	routes        []config.RouteConfig
	serviceMap    map[string]*config.ServiceConfig
	globalConfig  *config.GlobalConfig
	pluginsConfig *config.PluginsConfig   // To pass global plugin settings
	proxyHandler  http.Handler            // The ultimate handler (reverse proxy)
	routeHandlers map[string]http.Handler // Stores pre-built plugin chains per route ID
}

// NewRouter creates a new Router instance.
// The proxyHandler will be the next step in the chain (e.g., the reverse proxy).
func NewRouter(cfg *config.Config, proxyHandler http.Handler) *Router {

	serviceMap := make(map[string]*config.ServiceConfig)

	for i := range cfg.Services {
		serviceMap[cfg.Services[i].Name] = &cfg.Services[i]
	}

	if proxyHandler == nil {
		// This is a critical dependency. If it's nil, routing won't lead anywhere.
		logging.Fatal("Proxy handler cannot be nil when creating a new router.")
	}

	rt := &Router{
		routes:        cfg.Routes,
		serviceMap:    serviceMap,
		globalConfig:  &cfg.Global,
		pluginsConfig: &cfg.Plugins, // Pass global plugin settings
		proxyHandler:  proxyHandler, // Store the proxyHandler
		routeHandlers: make(map[string]http.Handler),
	}

	// Pre-build plugin chains for each route
	rt.buildRouteHandlers(proxyHandler)
	return rt

}

// buildRouteHandlers pre-constructs the handler chain for each route.
func (rt *Router) buildRouteHandlers(proxyHandler http.Handler) {

	for i := range rt.routes {

		route := &rt.routes[i]
		chainedHandler, err := rt.buildPluginChain(route, proxyHandler)

		if err != nil {
			logging.Errorf("Router: Failed to build plugin chain for route %s: %v. Route will be skipped.", route.ID, err)
			continue // Skip this route
		}

		rt.routeHandlers[route.ID] = chainedHandler
		logging.Infof("Router: Built plugin chain for route %s", route.ID)

	}

}

// buildPluginChain creates the handler chain for a single route.
func (rt *Router) buildPluginChain(route *config.RouteConfig, proxyHandler http.Handler) (http.Handler, error) {

	var activePlugins []plugins.Plugin

	if len(route.Plugins) > 0 {

		for _, pluginInstanceConfig := range route.Plugins {

			instance, err := plugins.GetPluginInstance(&pluginInstanceConfig, rt.pluginsConfig)

			if err != nil {
				// Return the error to be handled by the caller (buildRouteHandlers)
				return nil, fmt.Errorf("failed to get plugin instance '%s' for route '%s': %w", pluginInstanceConfig.Name, route.ID, err)
			}

			activePlugins = append(activePlugins, instance)

		}

	}

	// Create the handler chain: plugins wrap the proxyHandler
	// The plugins.ChainPlugins function itself logs which plugins are chained.
	chainedHandler := plugins.ChainPlugins(activePlugins, proxyHandler)
	return chainedHandler, nil

}

// ServeHTTP implements the http.Handler interface.
// It finds a matching route and then delegates to the proxyHandler.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	requestPath := r.URL.Path
	requestMethod := r.Method
	requestHost := r.Host // r.Host includes port if specified by client

	// If r.Host is empty (e.g. HTTP/1.0 requests), try r.URL.Host
	// However, for server-side routing, r.Host (from Host header) is typical.
	// For simplicity, we'll primarily use r.Host.

	logging.Debugf("Router: Attempting to match request: %s %s%s", requestMethod, requestHost, requestPath)

	for i := range rt.routes {

		route := &rt.routes[i] // Use a pointer to avoid copying the struct

		// 1. Match Host
		if route.Host != "" {

			hostname := requestHost
			if strings.Contains(hostname, ":") {
				hostname = strings.Split(hostname, ":")[0]
			}

			hostMatch := false
			if strings.HasPrefix(route.Host, "*.") {

				// Wildcard host matching e.g., *.example.com
				wildcardBase := strings.TrimPrefix(route.Host, "*.")

				// Request hostname must end with .wildcardBase and not be equal to wildcardBase itself
				if hostname != wildcardBase && strings.HasSuffix(hostname, "."+wildcardBase) {
					hostMatch = true
				}

			} else {

				// Exact host matching
				if route.Host == hostname {
					hostMatch = true
				}

			}

			if !hostMatch {
				logging.Debugf("Router: Host mismatch for route %s: route host pattern '%s', request host '%s' (normalized to '%s')", route.ID, route.Host, requestHost, hostname)
				continue
			}

		}

		// 2. Match Method
		if len(route.Methods) > 0 {

			methodMatch := false

			for _, method := range route.Methods {
				if strings.EqualFold(method, requestMethod) { // Case-insensitive method match
					methodMatch = true
					break
				}
			}

			if !methodMatch {
				logging.Debugf("Router: Method mismatch for route %s: expected one of %v, got %s", route.ID, route.Methods, requestMethod)
				continue
			}

		}

		// 3. Match Path
		pathMatch := false
		for _, routePathPattern := range route.Paths {
			if matchPath(requestPath, routePathPattern) {
				pathMatch = true
				break
			}
		}

		if !pathMatch {
			logging.Debugf("Router: Path mismatch for route %s: request path %s did not match patterns %v", route.ID, requestPath, route.Paths)
			continue
		}

		// If all conditions match, this is our route.
		serviceCfg, ok := rt.serviceMap[route.Service]
		if !ok {
			logging.Errorf("Router: Route %s points to undefined service %s. Skipping.", route.ID, route.Service)
			continue // Should be caught by config validation, but good to be safe.
		}

		logging.Infof("Router: Matched route %s to service %s for request %s %s%s", route.ID, serviceCfg.Name, requestMethod, requestHost, requestPath)

		// Store matched route and service info in context for downstream handlers (proxy, plugins)
		matchedInfo := &MatchedRouteInfo{
			RouteConfig:   route,
			ServiceConfig: serviceCfg,
		}

		ctx := context.WithValue(r.Context(), MatchedRouteKey, matchedInfo)
		r = r.WithContext(ctx)

		// Get the pre-built chain for this route.
		chainedHandler, ok := rt.routeHandlers[route.ID]
		if !ok {
			// This should not happen if buildRouteHandlers is correct.
			logging.Errorf("Router: No pre-built handler chain found for route %s. Using fallback (no plugins).", route.ID)
			// Proceed without plugins. In a stricter app, you might return a 500 error.
			rt.proxyHandler.ServeHTTP(w, r)
			return
		}
		// Execute the chain
		chainedHandler.ServeHTTP(w, r)
		return
	}

	// No route matched
	logging.Warnf("Router: No route matched for request: %s %s%s", requestMethod, requestHost, requestPath)
	http.NotFound(w, r)

}

// matchPath checks if the requestPath matches the routePathPattern.
// Supports a trailing wildcard "*" in the routePathPattern.
// e.g., "/api/v1/users/*" matches "/api/v1/users/123" and "/api/v1/users/profile/settings"
// e.g., "/api/v1/products" matches only "/api/v1/products"
func matchPath(requestPath, routePathPattern string) bool {

	if strings.HasSuffix(routePathPattern, "/*") {

		prefix := strings.TrimSuffix(routePathPattern, "/*")
		// Exact match for the part before /* or prefix match
		if requestPath == prefix || strings.HasPrefix(requestPath, prefix+"/") {
			return true
		}

	} else if strings.HasSuffix(routePathPattern, "*") && !strings.HasSuffix(routePathPattern, "/*") {

		// General wildcard, treat as prefix match
		prefix := strings.TrimSuffix(routePathPattern, "*")
		if strings.HasPrefix(requestPath, prefix) {
			return true
		}

	}

	// Exact match if no wildcard
	return requestPath == routePathPattern

}
