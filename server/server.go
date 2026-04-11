// Package server wires up the HTTP server, TLS configuration, and all route
// handlers for vm-builder-agent.
package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/tlhakhan/vm-builder-agent/jobs"
	"github.com/tlhakhan/vm-builder-agent/runner"
)

// Config holds everything the server needs to start.
type Config struct {
	ListenAddr string // e.g. ":8443"
	MTLS       bool
	CertFile   string
	KeyFile    string
	CAFile     string
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
		tlsCfg, err := buildTLSConfig(cfg.CertFile, cfg.KeyFile, cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls config: %w", err)
		}
		srv.TLSConfig = tlsCfg
	}

	return srv, nil
}

// buildTLSConfig loads the server cert/key and CA pool for mTLS.
func buildTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load cert/key: %w", err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert from %s", caFile)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    caPool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
