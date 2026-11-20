package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	logger := log.New(os.Stderr, "[rayneo-broker] ", log.LstdFlags|log.LUTC)

	defaultListen := envOrDefault("BROKER_LISTEN", ":8787")
	defaultState := defaultStatePath()
	defaultRequireHTTPS := strings.EqualFold(strings.TrimSpace(os.Getenv("BROKER_REQUIRE_HTTPS")), "true") ||
		strings.TrimSpace(os.Getenv("BROKER_REQUIRE_HTTPS")) == "1"

	listenAddr := flag.String("listen", defaultListen, "HTTP listen address (env: BROKER_LISTEN)")
	stateFile := flag.String("state", defaultState, "Path to persistent JSON state file with 0600 perms (env: BROKER_STATE_FILE)")
	requireHTTPS := flag.Bool("require-https", defaultRequireHTTPS, "Reject non-HTTPS requests without X-Forwarded-Proto: https (env: BROKER_REQUIRE_HTTPS)")
	flag.Parse()

	secrets := SecretSet{
		Shared: os.Getenv("BROKER_SECRET"),
		Read:   os.Getenv("BROKER_READ_SECRET"),
		Write:  os.Getenv("BROKER_WRITE_SECRET"),
	}
	if ok, reason := secrets.Validate(); !ok {
		if reason == "weak_or_placeholder_secret" {
			logger.Fatal("configured broker secret is too weak or a placeholder (minimum 32 random chars, e.g. `openssl rand -hex 32`)")
		}
		logger.Fatal("BROKER_SECRET (or BROKER_READ_SECRET / BROKER_WRITE_SECRET) environment variable is required (refusing to start without authentication)")
	}

	resolvedState := expandHome(strings.TrimSpace(*stateFile))
	store, err := NewStore(resolvedState)
	if err != nil {
		logger.Fatalf("failed to initialize state store (%s): %v", resolvedState, err)
	}

	broker := NewBrokerWithSecrets(store, secrets, logger)
	broker.requireHTTPS = *requireHTTPS

	srv := &http.Server{
		Addr:              strings.TrimSpace(*listenAddr),
		Handler:           broker,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 16,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Printf("listening on %s (state=%s)", srv.Addr, resolvedState)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		if err != nil {
			logger.Fatalf("server error: %v", err)
		}
	case <-ctx.Done():
		logger.Printf("shutdown signal received, draining connections...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Fatalf("graceful shutdown failed: %v", err)
		}
		logger.Printf("shutdown complete")
	}
}

func envOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func defaultStatePath() string {
	if v := strings.TrimSpace(os.Getenv("BROKER_STATE_FILE")); v != "" {
		return v
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "rayneo-broker", "state.json")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "rayneo-broker", "state.json")
	}
	return "./broker-state.json"
}

func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	return filepath.Join(home, p[2:])
}
