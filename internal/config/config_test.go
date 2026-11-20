package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveBrokerAndHasBroker(t *testing.T) {
	t.Setenv("RAYNEO_BROKER_URL", "")
	t.Setenv("RAYNEO_BROKER_SECRET", "")

	c := &Config{}
	if c.HasBroker() {
		t.Fatalf("expected HasBroker=false on empty config")
	}

	c.BrokerURL = "https://broker.example.com/"
	c.BrokerSecret = "cfg-secret"
	if !c.HasBroker() {
		t.Fatalf("expected HasBroker=true with stored URL and secret")
	}
	u, s := c.EffectiveBroker()
	if u != "https://broker.example.com" || s != "cfg-secret" {
		t.Fatalf("unexpected EffectiveBroker: url=%q secret=%q", u, s)
	}

	// Env vars override config values.
	t.Setenv("RAYNEO_BROKER_URL", " https://env-broker.example.com/// ")
	t.Setenv("RAYNEO_BROKER_SECRET", " env-secret ")
	u, s = c.EffectiveBroker()
	if u != "https://env-broker.example.com" || s != "env-secret" {
		t.Fatalf("expected env override, got url=%q secret=%q", u, s)
	}

	// Partial env override falls back to config for the unset piece.
	t.Setenv("RAYNEO_BROKER_URL", "")
	u, s = c.EffectiveBroker()
	if u != "https://broker.example.com" || s != "env-secret" {
		t.Fatalf("expected partial fallback, got url=%q secret=%q", u, s)
	}
}

func TestCanRefreshDisabledByNoRefreshOrBroker(t *testing.T) {
	t.Setenv("RAYNEO_BROKER_URL", "")
	t.Setenv("RAYNEO_BROKER_SECRET", "")

	c := &Config{RefreshToken: "rt_dummy"}
	if !c.CanRefresh() {
		t.Fatalf("expected CanRefresh=true when NoRefresh=false and no broker")
	}

	c.NoRefresh = true
	if c.CanRefresh() {
		t.Fatalf("expected CanRefresh=false when NoRefresh=true")
	}

	c.NoRefresh = false
	c.BrokerURL = "https://broker.example.com"
	c.BrokerSecret = "secret"
	if c.CanRefresh() {
		t.Fatalf("expected CanRefresh=false when broker is configured")
	}
}

func TestSaveAndLoadBrokerFields(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("RAYNEO_BROKER_URL", "")
	t.Setenv("RAYNEO_BROKER_SECRET", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c.Token = "tok_test_1234567890"
	c.NoRefresh = true
	c.BrokerURL = "https://broker.example.com"
	c.BrokerSecret = "sec_test_1234567890"
	c.BrokerETag = `"rev-1-abcdef123456"`
	c.TokenExpiresAt = "2026-11-20T13:30:00Z"
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify 0600 file permissions.
	cfgPath := filepath.Join(tmp, "rayneo-cli", "config.json")
	st, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("Stat config.json: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("expected 0600 perms on config.json, got %04o", perm)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.BrokerURL != c.BrokerURL || got.BrokerSecret != c.BrokerSecret ||
		got.BrokerETag != c.BrokerETag || got.TokenExpiresAt != c.TokenExpiresAt ||
		!got.NoRefresh || got.RefreshToken != "" {
		t.Fatalf("reloaded config mismatch: %+v", got)
	}
}
