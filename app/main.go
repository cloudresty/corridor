package main

import (
	"flag"
	"fmt"
	stlog "log" // Standard logger for critical startup errors
	"os"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/healthchecker"
	"github.com/cloudresty/corridor/logging"
	"github.com/cloudresty/corridor/proxy"
	"github.com/cloudresty/corridor/router"
	"github.com/cloudresty/corridor/server"
	"github.com/cloudresty/corridor/utils"

	_ "github.com/cloudresty/corridor/plugins" // This will run init in plugin.go
)

var (
	configPath string
)

func init() {

	// Let's assume for now it's in the root of the project
	// where the binary might be run from, or a 'config' subdir.
	flag.StringVar(&configPath, "config", "config.yaml", "Path to the configuration file")

}

func main() {

	// Check for 'keygen' subcommand first, before flag parsing for the main app
	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "Usage: corridor keygen \"<plaintext_api_key>\"")
			fmt.Fprintln(os.Stderr, "Ensure the API key is quoted if it contains spaces or special characters.")
			os.Exit(1)
		}
		plaintextKey := os.Args[2]
		lookupKey, bcryptHash, err := utils.GenerateAPIKeyHashes(plaintextKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error generating key hashes: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("--- Generated API Key Details ---\n")
		fmt.Printf("Plaintext API Key: %s\n", plaintextKey)
		fmt.Printf("  Lookup Key (SHA256 Hex for 'lookup_key' field): %s\n", lookupKey)
		fmt.Printf("  Hashed Value (bcrypt for 'hashed_value' field): %s\n\n", bcryptHash)
		fmt.Println("Instructions:")
		fmt.Println("1. Provide the 'Plaintext API Key' to your client. Corridor does NOT store this.")
		fmt.Println("2. Store the 'Lookup Key' and 'Hashed Value' in your Corridor configuration (e.g., config.yaml or MongoDB).")
		os.Exit(0)
	}

	flag.Parse()

	// 1. Load Configuration
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		stlog.Fatalf("CRITICAL: Failed to load configuration from '%s': %v", configPath, err)
	}

	// 2. Initialize Logger
	// The Init function in our custom logger takes globalCfg and pluginLoggingCfg
	logging.Init(&cfg.Global, cfg.Plugins.Logging)

	// Optionally enable caller reporting for debugging during development
	// logging.SetReportCaller(true)

	logging.Info("Corridor API Gateway starting...")
	logging.Infof("Configuration loaded from: %s", configPath)
	logging.Debugf("Global config: %+v", cfg.Global)

	// 3. Initialize Proxy
	// The proxy will internally create load balancers for each service.
	proxyHandler, err := proxy.NewProxy(cfg)
	if err != nil {
		logging.Fatalf("Failed to initialize proxy: %v", err)
	}
	logging.Info("Proxy initialized.")

	// 4. Initialize and Start Health Checker
	// The health checker needs a reference to the proxy to update target health.
	hc := healthchecker.NewHealthChecker(cfg, proxyHandler)
	hc.Start() // Start health checks in background goroutines
	logging.Info("Health checker started.")

	// 5. Initialize Router
	// The router needs the main proxy handler to delegate requests to after matching and plugin processing.
	mainRouter := router.NewRouter(cfg, proxyHandler)
	logging.Info("Router initialized.")

	// 6. Initialize Server
	// The server takes the main router as its primary handler.
	gatewayServer := server.NewServer(cfg, mainRouter)
	logging.Info("Gateway server initialized.")

	// 7. Start Server
	// This will block until a shutdown signal is received.
	// The Start method itself handles graceful shutdown of the HTTP server.
	if err := gatewayServer.Start(); err != nil {
		logging.Errorf("Gateway server failed to start or shut down cleanly: %v", err)
	}

	// 8. Graceful shutdown for Health Checker
	// This happens after the server's Start() method returns (i.e., after shutdown signal)
	hc.Stop()
	logging.Info("Health checker gracefully stopped.")

	logging.Info("Corridor API Gateway has shut down.")
}
