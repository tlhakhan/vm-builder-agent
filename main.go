package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tlhakhan/vm-builder-agent/jobs"
	"github.com/tlhakhan/vm-builder-agent/runner"
	"github.com/tlhakhan/vm-builder-agent/server"
)

func main() {
	// ── flags ──────────────────────────────────────────────────────────────
	listenAddr   := flag.String("listen", ":8443", "address to listen on")
	mtls         := flag.Bool("mtls", false, "enable mTLS (requires --cert, --key, --ca)")
	certFile     := flag.String("cert", "certs/server.crt", "server TLS certificate file")
	keyFile      := flag.String("key", "certs/server.key", "server TLS key file")
	caFile       := flag.String("ca", "certs/ca.crt", "CA certificate file for client verification")
	clientCN     := flag.String("client-cn", "vm-api-server", "expected client certificate CN (mTLS only)")
	coreRepo      := flag.String("core-repo", "", "git URL for vm-builder-core (required)")
	terraformBin  := flag.String("terraform", "tofu", "terraform/opentofu binary name or path")
	workspacesDir := flag.String("workspaces-dir", "/var/lib/vm-builder-agent/workspaces", "directory where per-VM terraform workspaces are kept")
	flag.Parse()

	if *coreRepo == "" {
		slog.Error("--core-repo is required")
		os.Exit(1)
	}

	// ── structured logging ─────────────────────────────────────────────────
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	hostname, _ := os.Hostname()
	slog.Info("starting vm-builder-agent",
		"host", hostname,
		"listen", *listenAddr,
		"mtls", *mtls,
		"core_repo", *coreRepo,
		"terraform_bin", *terraformBin,
		"workspaces_dir", *workspacesDir,
	)

	// ── dependencies ───────────────────────────────────────────────────────
	tracker := jobs.NewTracker()

	r := runner.New(runner.Config{
		CoreRepoURL:   *coreRepo,
		TerraformBin:  *terraformBin,
		WorkspacesDir: *workspacesDir,
	})

	srv, err := server.New(server.Config{
		ListenAddr: *listenAddr,
		MTLS:       *mtls,
		CertFile:   *certFile,
		KeyFile:    *keyFile,
		CAFile:     *caFile,
		ClientCN:   *clientCN,
	}, tracker, r)
	if err != nil {
		slog.Error("failed to create server", "err", err)
		os.Exit(1)
	}

	// ── serve ──────────────────────────────────────────────────────────────
	errCh := make(chan error, 1)
	go func() {
		if *mtls {
			slog.Info("listening with mTLS", "addr", *listenAddr)
			// TLSConfig is already set; pass empty strings to use it.
			errCh <- srv.ListenAndServeTLS("", "")
		} else {
			slog.Info("listening without TLS", "addr", *listenAddr)
			errCh <- srv.ListenAndServe()
		}
	}()

	// ── graceful shutdown ──────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-quit:
		slog.Info("received signal, shutting down", "signal", sig)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			slog.Error("server error", "err", err)
			os.Exit(1)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
		os.Exit(1)
	}
	slog.Info("shutdown complete")
}
