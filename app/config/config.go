package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the entire gateway configuration
type Config struct {
	Global   GlobalConfig    `yaml:"global"`
	Services []ServiceConfig `yaml:"services"`
	Routes   []RouteConfig   `yaml:"routes"`
	Plugins  PluginsConfig   `yaml:"plugins"`
	TLS      *TLSConfig      `yaml:"tls,omitempty"` // Optional
}

// GlobalConfig defines global settings for the gateway
type GlobalConfig struct {
	Port             int      `yaml:"port"`
	LogLevel         string   `yaml:"log_level"`
	EnableHTTPS      bool     `yaml:"enable_https"`
	DefaultTimeoutMs int      `yaml:"default_timeout_ms"`
	ProxyHeaders     []string `yaml:"proxy_headers"`
	// Server-specific timeouts (in seconds)
	ReadTimeoutSec         *int  `yaml:"read_timeout_sec"`           // Optional: time to read entire request, including body
	WriteTimeoutSec        *int  `yaml:"write_timeout_sec"`          // Optional: time to write response
	IdleTimeoutSec         *int  `yaml:"idle_timeout_sec"`           // Optional: max time to wait for next request on a keep-alive connection
	ReadHeaderTimeoutSec   *int  `yaml:"read_header_timeout_sec"`    // Optional: time to read request headers
	FailOnServiceInitError *bool `yaml:"fail_on_service_init_error"` // Optional: If true, gateway fails to start if any service LB can't be initialized. Defaults to false.
	// HTTP Client Transport specific timeouts (for proxy to upstream)
	TransportDialTimeoutSec           *int `yaml:"transport_dial_timeout_sec"`            // Optional: Connection timeout to upstream
	TransportKeepAliveSec             *int `yaml:"transport_keep_alive_sec"`              // Optional: Keep-alive period for upstream connections
	TransportMaxIdleConns             *int `yaml:"transport_max_idle_conns"`              // Optional: Max total idle connections to upstreams
	TransportMaxIdleConnsPerHost      *int `yaml:"transport_max_idle_conns_per_host"`     // Optional: Max idle connections per upstream host
	TransportIdleConnTimeoutSec       *int `yaml:"transport_idle_conn_timeout_sec"`       // Optional: Timeout for idle connections to upstreams
	TransportTLSHandshakeTimeoutSec   *int `yaml:"transport_tls_handshake_timeout_sec"`   // Optional: TLS handshake timeout with upstream
	TransportExpectContinueTimeoutSec *int `yaml:"transport_expect_continue_timeout_sec"` // Optional: Timeout for expecting 100-continue from upstream
	TransportResponseHeaderTimeoutSec *int `yaml:"transport_response_header_timeout_sec"` // Optional: Timeout for receiving response headers from upstream (overrides default_timeout_ms for this specific case)

}

// ServiceConfig defines a backend service
type ServiceConfig struct {
	Name          string               `yaml:"name"`
	UpstreamURLs  []string             `yaml:"upstream_urls,omitempty"` // Used if no specific targets in load_balancing
	UpstreamURL   string               `yaml:"upstream_url,omitempty"`  // For single instance services
	LoadBalancing *LoadBalancingConfig `yaml:"load_balancing,omitempty"`
}

// LoadBalancingConfig defines load balancing settings for a service
type LoadBalancingConfig struct {
	Strategy     string             `yaml:"strategy"`
	Targets      []TargetConfig     `yaml:"targets,omitempty"` // Used for weighted or specific target definitions
	HealthChecks *HealthCheckConfig `yaml:"health_checks,omitempty"`
}

// TargetConfig defines a specific upstream target, potentially with a weight
type TargetConfig struct {
	URL    string `yaml:"url"`
	Weight int    `yaml:"weight,omitempty"` // For weighted strategies
}

// HealthCheckConfig defines health check parameters
type HealthCheckConfig struct {
	Ready              string `yaml:"ready"`
	Live               string `yaml:"live"`
	Interval           int    `yaml:"interval"` // in seconds
	Timeout            int    `yaml:"timeout"`  // in seconds
	UnhealthyThreshold int    `yaml:"unhealthy_threshold"`
	HealthyThreshold   int    `yaml:"healthy_threshold"`
}

// RouteConfig defines a routing rule
type RouteConfig struct {
	ID      string                 `yaml:"id"`
	Paths   []string               `yaml:"paths"`
	Methods []string               `yaml:"methods,omitempty"` // Optional, defaults to all if empty
	Host    string                 `yaml:"host,omitempty"`    // Optional
	Service string                 `yaml:"service"`           // Name of the service to route to
	Plugins []PluginInstanceConfig `yaml:"plugins,omitempty"`
}

// PluginInstanceConfig defines an instance of a plugin applied to a route
type PluginInstanceConfig struct {
	Name   string         `yaml:"name"`
	Config map[string]any `yaml:"config,omitempty"`
}

// PluginsConfig defines global plugin configurations/defaults
type PluginsConfig struct {
	Authentication map[string]any `yaml:"authentication,omitempty"`
	RateLimiting   map[string]any `yaml:"rate-limiting,omitempty"`
	Caching        map[string]any `yaml:"caching,omitempty"`
	Logging        map[string]any `yaml:"logging,omitempty"`
}

// TLSConfig defines TLS settings
type TLSConfig struct {
	Certificates []CertificateConfig `yaml:"certificates"`
}

// CertificateConfig defines a TLS certificate
type CertificateConfig struct {
	Hostname string `yaml:"hostname"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// LoadConfig reads the configuration file from the given path and unmarshals it.
func LoadConfig(configPath string) (*Config, error) {
	configFile, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", configPath, err)
	}

	var cfg Config
	err = yaml.Unmarshal(configFile, &cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal config file %s: %w", configPath, err)
	}

	// Apply default values and perform initial validation
	if err := validateAndSetDefaults(&cfg); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &cfg, nil
}

// validateAndSetDefaults performs basic validation and sets default values.
func validateAndSetDefaults(cfg *Config) error {

	if cfg.Global.Port == 0 {
		cfg.Global.Port = 8080 // Default port
	}

	if cfg.Global.LogLevel == "" {
		cfg.Global.LogLevel = "info"
	}

	if cfg.Global.DefaultTimeoutMs == 0 {
		cfg.Global.DefaultTimeoutMs = 5000 // Default timeout 5 seconds
	}

	if cfg.Global.EnableHTTPS {

		if cfg.TLS == nil || len(cfg.TLS.Certificates) == 0 {
			return fmt.Errorf("HTTPS is enabled but no TLS certificates are configured")
		}

		for i, cert := range cfg.TLS.Certificates {

			if cert.Hostname == "" {
				return fmt.Errorf("TLS certificate %d: hostname is required", i)
			}

			if cert.CertFile == "" {
				return fmt.Errorf("TLS certificate %d for hostname %s: cert_file is required", i, cert.Hostname)
			}

			if cert.KeyFile == "" {
				return fmt.Errorf("TLS certificate %d for hostname %s: key_file is required", i, cert.Hostname)
			}

		}

	}

	serviceNames := make(map[string]bool)
	for i, service := range cfg.Services {
		if service.Name == "" {
			return fmt.Errorf("service %d: name is required", i)
		}
		if serviceNames[service.Name] {
			return fmt.Errorf("service name %s is not unique", service.Name)
		}
		serviceNames[service.Name] = true

		if len(service.UpstreamURLs) == 0 && service.UpstreamURL == "" && (service.LoadBalancing == nil || len(service.LoadBalancing.Targets) == 0) {
			return fmt.Errorf("service %s: must have upstream_urls, upstream_url, or load_balancing.targets defined", service.Name)
		}
		if service.UpstreamURL != "" && (len(service.UpstreamURLs) > 0 || (service.LoadBalancing != nil && len(service.LoadBalancing.Targets) > 0)) {
			return fmt.Errorf("service %s: use either 'upstream_url' for a single instance or 'upstream_urls'/'load_balancing.targets' for multiple, not both", service.Name)
		}

		// If upstream_url is provided, and no load_balancing.targets, populate targets for consistency
		if service.UpstreamURL != "" && (service.LoadBalancing == nil || len(service.LoadBalancing.Targets) == 0) {

			if service.LoadBalancing == nil {
				cfg.Services[i].LoadBalancing = &LoadBalancingConfig{} // Initialize if nil
			}
			// If strategy is not set, and it's a single upstream, it doesn't strictly need one,
			// but for internal consistency, we can assume a default or leave it.
			// For now, we'll just ensure targets are populated if upstream_url is used.
			cfg.Services[i].LoadBalancing.Targets = []TargetConfig{{URL: service.UpstreamURL}}
			cfg.Services[i].UpstreamURL = "" // Clear the single URL field after processing

		} else if len(service.UpstreamURLs) > 0 && (service.LoadBalancing == nil || len(service.LoadBalancing.Targets) == 0) {

			// If upstream_urls are provided, and no load_balancing.targets, populate targets
			if service.LoadBalancing == nil {
				cfg.Services[i].LoadBalancing = &LoadBalancingConfig{} // Initialize if nil
			}

			if cfg.Services[i].LoadBalancing.Strategy == "" {
				cfg.Services[i].LoadBalancing.Strategy = "round-robin" // Default strategy if multiple upstreams and none specified
			}

			targets := make([]TargetConfig, len(service.UpstreamURLs))
			for j, url := range service.UpstreamURLs {
				targets[j] = TargetConfig{URL: url}
			}

			cfg.Services[i].LoadBalancing.Targets = targets
			cfg.Services[i].UpstreamURLs = nil // Clear the URLs field after processing

		}

		if service.LoadBalancing != nil {

			if len(service.LoadBalancing.Targets) == 0 {
				return fmt.Errorf("service %s: load_balancing.targets cannot be empty if load_balancing is defined", service.Name)
			}

			if service.LoadBalancing.Strategy == "" {

				// If only one target, strategy is not strictly necessary, but good to have a default.
				// If multiple targets, strategy is essential.
				if len(service.LoadBalancing.Targets) > 1 {
					return fmt.Errorf("service %s: load_balancing.strategy is required when multiple targets are defined", service.Name)
				}

				// For a single target, any strategy behaves the same. We can set a default.
				cfg.Services[i].LoadBalancing.Strategy = "round-robin"

			}

			// Validate strategies
			validStrategies := map[string]bool{"round-robin": true, "least-connections": true, "random": true, "weighted": true}
			if !validStrategies[service.LoadBalancing.Strategy] {
				return fmt.Errorf("service %s: invalid load_balancing.strategy '%s'", service.Name, service.LoadBalancing.Strategy)
			}

			if service.LoadBalancing.HealthChecks != nil {

				hc := service.LoadBalancing.HealthChecks

				if hc.Interval <= 0 {
					hc.Interval = 30 // Default interval
				}

				if hc.Timeout <= 0 {
					hc.Timeout = 5 // Default timeout
				}

				if hc.UnhealthyThreshold <= 0 {
					hc.UnhealthyThreshold = 2
				}

				if hc.HealthyThreshold <= 0 {
					hc.HealthyThreshold = 1
				}

				if hc.Ready == "" && hc.Live == "" {
					return fmt.Errorf("service %s: at least one of health_checks.ready or health_checks.live must be defined", service.Name)
				}

			}
		}
	}

	routeIDs := make(map[string]bool)

	for i, route := range cfg.Routes {

		if route.ID == "" {
			return fmt.Errorf("route %d: id is required", i)
		}

		if routeIDs[route.ID] {
			return fmt.Errorf("route id %s is not unique", route.ID)
		}

		routeIDs[route.ID] = true

		if len(route.Paths) == 0 {
			return fmt.Errorf("route %s: paths are required", route.ID)
		}

		if route.Service == "" {
			return fmt.Errorf("route %s: service is required", route.ID)
		}

		if !serviceNames[route.Service] {
			return fmt.Errorf("route %s: service '%s' not found in service definitions", route.ID, route.Service)
		}
		// Further validation for methods, host patterns, etc. can be added here.
	}

	// Validate plugin configurations (basic existence for now)
	// More detailed validation would happen when plugins are loaded/initialized.

	return nil

}

// GetDefaultTimeout returns the default request timeout as a time.Duration
func (gc *GlobalConfig) GetDefaultTimeout() time.Duration {
	if gc == nil || gc.DefaultTimeoutMs <= 0 {
		return 10 * time.Second // Default to 10 seconds if not specified or invalid
	}
	return time.Duration(gc.DefaultTimeoutMs) * time.Millisecond
}
