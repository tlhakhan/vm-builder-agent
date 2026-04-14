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

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	// ── flags ──────────────────────────────────────────────────────────────
	listenAddr      := flag.String("listen", ":8443", "address to listen on")
	agentMTLS       := flag.Bool("agent-mtls", false, "enable mTLS for the agent listener (requires --agent-trusted-ca-url)")
	agentCAURL      := flag.String("agent-trusted-ca-url", "", "URL to the CA certificate used to verify agent client certificates")
	privateDir      := flag.String("private-dir", "/etc/vm-builder-agent/private", "directory where the agent stores its self-signed TLS certificate and private key")
	clientCN        := flag.String("agent-authorized-client-cn", "vm-builder-apiserver", "expected client certificate CN for agent mTLS, signed by the trusted CA")
	coreRepo        := flag.String("core-repo", "", "git URL for vm-builder-core (required)")
	terraformBin    := flag.String("terraform", "tofu", "terraform/opentofu binary name or path")
	workspacesDir   := flag.String("workspaces-dir", "/var/lib/vm-builder-agent/workspaces", "directory where per-VM terraform workspaces are kept")
	cloudImageCache := flag.String("cloud-image-cache-dir", "/var/lib/vm-builder-agent/cloud-image-cache", "directory where cloud images are cached to avoid repeated downloads")
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
		"version", version,
		"host", hostname,
		"listen", *listenAddr,
		"agent_mtls", *agentMTLS,
		"agent_trusted_ca_url", *agentCAURL,
		"private_dir", *privateDir,
		"core_repo", *coreRepo,
		"terraform_bin", *terraformBin,
		"workspaces_dir", *workspacesDir,
		"cloud_image_cache_dir", *cloudImageCache,
	)

	// ── dependencies ───────────────────────────────────────────────────────
	tracker := jobs.NewTracker()

	r := runner.New(runner.Config{
		CoreRepoURL:        *coreRepo,
		TerraformBin:       *terraformBin,
		WorkspacesDir:      *workspacesDir,
		CloudImageCacheDir: *cloudImageCache,
	})

	srv, err := server.New(server.Config{
		ListenAddr: *listenAddr,
		MTLS:       *agentMTLS,
		CAURL:      *agentCAURL,
		PrivateDir: *privateDir,
		ClientCN:   *clientCN,
	}, tracker, r)
	if err != nil {
		slog.Error("failed to create server", "err", err)
		os.Exit(1)
	}

	// ── serve ──────────────────────────────────────────────────────────────
	errCh := make(chan error, 1)
	go func() {
		if *agentMTLS {
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
