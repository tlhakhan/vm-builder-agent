// Package server wires up the HTTP server, TLS configuration, and all route
// handlers for vm-builder-agent.
package server

import (
	"fmt"
	"net/http"

	"github.com/tlhakhan/vm-builder-agent/jobs"
	"github.com/tlhakhan/vm-builder-agent/runner"
)

// Config holds everything the server needs to start.
type Config struct {
	ListenAddr string // e.g. ":8443"
	MTLS       bool
	CAURL      string
	PrivateDir string
	// ClientCN is the Common Name that client certs must present when mTLS is on.
	ClientCN string
}

// New builds an *http.Server with routing and optional mTLS already configured.
func New(cfg Config, tracker *jobs.Tracker, r *runner.Runner) (*http.Server, error) {
	mux := http.NewServeMux()
	h := &handlers{tracker: tracker, runner: r}

	mux.HandleFunc("POST /vm/create", h.createVM)
	mux.HandleFunc("DELETE /vm/{name}", h.deleteVM)
	mux.HandleFunc("GET /vm", h.listVMs)
	mux.HandleFunc("GET /vm/{name}", h.getVM)
	mux.HandleFunc("POST /vm/{name}/start", h.startVM)
	mux.HandleFunc("POST /vm/{name}/shutdown", h.shutdownVM)
	mux.HandleFunc("GET /jobs/{id}", h.getJob)
	mux.HandleFunc("GET /node", h.nodeInfo)
	mux.HandleFunc("GET /health", h.health)

	var root http.Handler = mux
	root = loggingMiddleware(root)
	if cfg.MTLS {
		root = cnMiddleware(cfg.ClientCN, root)
	}

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: root,
	}

	if cfg.MTLS {
		if cfg.CAURL == "" {
			return nil, fmt.Errorf("ca-url is required when mTLS is enabled")
		}
		tlsCfg, err := buildTLSConfig(cfg.PrivateDir, cfg.CAURL)
		if err != nil {
			return nil, fmt.Errorf("tls config: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}

	return srv, nil
}
