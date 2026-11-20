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

	listenAddr := flag.String("listen", defaultListen, "HTTP listen address (env: BROKER_LISTEN)")
	stateFile := flag.String("state", defaultState, "Path to persistent JSON state file with 0600 perms (env: BROKER_STATE_FILE)")
	flag.Parse()

	secret := strings.TrimSpace(os.Getenv("BROKER_SECRET"))
	if secret == "" {
		logger.Fatal("BROKER_SECRET environment variable is required (refusing to start without authentication)")
	}

	resolvedState := expandHome(strings.TrimSpace(*stateFile))
	store, err := NewStore(resolvedState)
	if err != nil {
		logger.Fatalf("failed to initialize state store (%s): %v", resolvedState, err)
	}

	broker := NewBroker(store, secret, logger)

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
