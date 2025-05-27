package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloudresty/corridor/config"
	"github.com/cloudresty/corridor/logging"
)

// GatewayServer represents the main API Gateway server
type GatewayServer struct {
	config     *config.Config
	httpServer *http.Server
	Handler    http.Handler // This will be our main router
}

// NewServer creates a new instance of the GatewayServer
// It takes an http.Handler which will be our router
func NewServer(cfg *config.Config, mainHandler http.Handler) *GatewayServer {

	if mainHandler == nil {

		// Fallback to a simple default if no handler is provided (should not happen in normal flow)
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			logging.Warn("Main handler not set in GatewayServer, using fallback.")
			http.NotFound(w, r)
		})

		mainHandler = mux

	}

	return &GatewayServer{
		config:  cfg,
		Handler: mainHandler,
	}

}

// Start begins listening for incoming HTTP(S) requests.
// It also handles graceful shutdown.
func (s *GatewayServer) Start() error {

	addr := fmt.Sprintf(":%d", s.config.Global.Port)

	// Determine server timeouts from config or use defaults
	readTimeout := 30 * time.Second
	if s.config.Global.ReadTimeoutSec != nil && *s.config.Global.ReadTimeoutSec > 0 {
		readTimeout = time.Duration(*s.config.Global.ReadTimeoutSec) * time.Second
	}

	writeTimeout := 30 * time.Second
	if s.config.Global.WriteTimeoutSec != nil && *s.config.Global.WriteTimeoutSec > 0 {
		writeTimeout = time.Duration(*s.config.Global.WriteTimeoutSec) * time.Second
	}

	idleTimeout := 60 * time.Second
	if s.config.Global.IdleTimeoutSec != nil && *s.config.Global.IdleTimeoutSec > 0 {
		idleTimeout = time.Duration(*s.config.Global.IdleTimeoutSec) * time.Second
	}

	readHeaderTimeout := 15 * time.Second
	if s.config.Global.ReadHeaderTimeoutSec != nil && *s.config.Global.ReadHeaderTimeoutSec > 0 {
		readHeaderTimeout = time.Duration(*s.config.Global.ReadHeaderTimeoutSec) * time.Second
	}

	s.httpServer = &http.Server{
		Addr:              addr,
		Handler:           s.Handler, // Use the provided main handler (our router)
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Channel to listen for OS signals for graceful shutdown
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)

	// Goroutine to start the server
	go func() {

		if s.config.Global.EnableHTTPS {

			if s.config.TLS == nil || len(s.config.TLS.Certificates) == 0 {
				// This should have been caught by config validation.
				logging.Fatalf("HTTPS is enabled, but no TLS certificates are configured.")
				return
			}
			logging.Infof("Starting HTTPS server on %s", addr)

			tlsConfig := &tls.Config{
				Certificates: make([]tls.Certificate, 0, len(s.config.TLS.Certificates)),
				MinVersion:   tls.VersionTLS12, // Recommended modern TLS version
			}

			for _, certConf := range s.config.TLS.Certificates {

				cert, err := tls.LoadX509KeyPair(certConf.CertFile, certConf.KeyFile)

				if err != nil {
					logging.Fatalf("Failed to load TLS certificate for %s (cert: %s, key: %s): %v", certConf.Hostname, certConf.CertFile, certConf.KeyFile, err)
				}

				tlsConfig.Certificates = append(tlsConfig.Certificates, cert)
				logging.Infof("Loaded TLS certificate for hostname: %s", certConf.Hostname)

			}

			s.httpServer.TLSConfig = tlsConfig

			// If TLSConfig is set, certFile and keyFile for ListenAndServeTLS can be empty.
			if err := s.httpServer.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
				logging.Fatalf("HTTPS server error: %v", err)
			}

		} else {

			logging.Infof("Starting HTTP server on %s", addr)

			if err := s.httpServer.ListenAndServe(); err != http.ErrServerClosed {
				logging.Fatalf("HTTP server error: %v", err)
			}

		}

	}()

	logging.Infof("Corridor API Gateway started. Press Ctrl+C to shut down.")

	// Block until a signal is received
	<-stopChan
	logging.Info("Shutting down server...")
	return s.Shutdown()

}

// Shutdown gracefully shuts down the server.
func (s *GatewayServer) Shutdown() error {

	timeout := s.config.Global.GetDefaultTimeout()

	if timeout <= 0 {
		timeout = 30 * time.Second // Fallback shutdown timeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			logging.Errorf("Server shutdown error: %v", err)
			return err
		}
	}

	logging.Info("Server gracefully stopped.")

	return nil

}
