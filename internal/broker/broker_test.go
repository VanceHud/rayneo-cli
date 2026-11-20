package broker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/config"
)

func TestFetch200And304(t *testing.T) {
	const testSecret = "super-secret-broker-key-1234"
	const testToken = "dummy_access_token_abcdef123456"
	const testETag = `"rev-3-deadbeef1234"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/token" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		if inm := r.Header.Get("If-None-Match"); inm == testETag {
			w.Header().Set("ETag", testETag)
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", testETag)
		// Deliberately include unsafe headers and a bogus refresh_token field to confirm stripping.
		resp := map[string]any{
			"ok":                true,
			"revision":          3,
			"access_token":      testToken,
			"refresh_token":     "must_be_ignored_rt",
			"token_fp":          TokenFingerprint(testToken),
			"source":            "loon:refresh",
			"captured_at":       "2026-11-20T11:30:00Z",
			"updated_at":        "2026-11-20T11:30:05Z",
			"expires_at":        "2026-11-20T13:30:05Z",
			"expires_estimated": false,
			"expired":           false,
			"remaining_seconds": 7150,
			"base_url":          "https://xr-api-p.rayneo.cn",
			"region":            "cn",
			"account":           "13800000000",
			"account_id":        12345,
			"device_id":         "00000000-0000-4000-8000-000000000099",
			"client_headers": map[string]string{
				"platform":          "ios",
				"Phone-Model":       "iPhone16,2",
				"xtcl-access-token": "forbidden",
				"authorization":     "forbidden",
				"host":              "forbidden",
				"content-type":      "forbidden",
				"accept-encoding":   "gzip",
				"cookie":            "forbidden",
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cl := &Client{HTTP: srv.Client()}

	// 1) Initial fetch (trailing slashes on URL should be trimmed cleanly).
	tr, notMod, err := cl.Fetch(context.Background(), srv.URL+"///", testSecret, "")
	if err != nil {
		t.Fatalf("Fetch 200 failed: %v", err)
	}
	if notMod {
		t.Fatalf("expected notModified=false on 200")
	}
	if tr == nil || tr.AccessToken != testToken || tr.Revision != 3 || tr.ETag != testETag {
		t.Fatalf("unexpected TokenResponse: %+v", tr)
	}
	if tr.Source != "loon:refresh" || tr.AccountID != 12345 || tr.RemainingSeconds != 7150 {
		t.Fatalf("unexpected metadata in TokenResponse: %+v", tr)
	}
	if len(tr.ClientHeaders) != 2 || tr.ClientHeaders["platform"] != "ios" || tr.ClientHeaders["phone-model"] != "iPhone16,2" {
		t.Fatalf("unexpected filtered ClientHeaders: %+v", tr.ClientHeaders)
	}

	// 2) Conditional fetch with matching ETag -> 304 Not Modified.
	tr304, notMod, err := cl.Fetch(context.Background(), srv.URL, testSecret, tr.ETag)
	if err != nil {
		t.Fatalf("Fetch 304 failed: %v", err)
	}
	if !notMod || tr304 != nil {
		t.Fatalf("expected (nil, true, nil) on 304, got (%+v, %v, %v)", tr304, notMod, err)
	}
}

func TestFetchErrorsAndSanitization(t *testing.T) {
	const testSecret = "secret-must-never-appear-in-errors-98765"

	t.Run("validation", func(t *testing.T) {
		if _, _, err := Fetch(context.Background(), "", testSecret, ""); err == nil {
			t.Fatalf("expected error on empty brokerURL")
		}
		if _, _, err := Fetch(context.Background(), "https://broker.example.com", "", ""); err == nil {
			t.Fatalf("expected error on empty secret")
		}
		if _, _, err := Fetch(context.Background(), "ftp://broker.example.com", testSecret, ""); err == nil {
			t.Fatalf("expected error on non-http scheme")
		}
	})

	t.Run("401 unauthorized", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		_, notMod, err := cl.Fetch(context.Background(), srv.URL, testSecret, "")
		if !errors.Is(err, ErrUnauthorized) || notMod {
			t.Fatalf("expected ErrUnauthorized, got notMod=%v err=%v", notMod, err)
		}
		if strings.Contains(err.Error(), testSecret) {
			t.Fatalf("secret leaked in 401 error: %v", err)
		}
	})

	t.Run("404 no token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":"no token synced yet"}`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		_, notMod, err := cl.Fetch(context.Background(), srv.URL, testSecret, "")
		if !errors.Is(err, ErrNoToken) || notMod {
			t.Fatalf("expected ErrNoToken, got notMod=%v err=%v", notMod, err)
		}
	})

	t.Run("500 sanitizes reflected secret", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"ok":false,"error":"bad secret ` + testSecret + `"}`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		_, _, err := cl.Fetch(context.Background(), srv.URL, testSecret, "")
		if err == nil {
			t.Fatalf("expected error on 500")
		}
		if strings.Contains(err.Error(), testSecret) {
			t.Fatalf("secret leaked in 500 error: %v", err)
		}
		if !strings.Contains(err.Error(), "[REDACTED]") {
			t.Fatalf("expected redacted marker in error, got %v", err)
		}
	})

	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{not-valid-json`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		if _, _, err := cl.Fetch(context.Background(), srv.URL, testSecret, ""); err == nil {
			t.Fatalf("expected error on malformed JSON")
		}
	})

	t.Run("empty access_token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true,"access_token":"   "}`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		if _, _, err := cl.Fetch(context.Background(), srv.URL, testSecret, ""); err == nil {
			t.Fatalf("expected error on empty access_token")
		}
	})

	t.Run("200 with ok=false", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":false,"error":"internal failure"}`))
		}))
		defer srv.Close()

		cl := &Client{HTTP: srv.Client()}
		if _, _, err := cl.Fetch(context.Background(), srv.URL, testSecret, ""); err == nil {
			t.Fatalf("expected error when ok=false")
		}
	})
}

func TestApplyToConfigEnforcesNoRefreshAndMergesMetadata(t *testing.T) {
	cfg := &config.Config{
		BaseURL:      "https://xr-api-p.rayneo.cn",
		Token:        "old_token",
		RefreshToken: "stale_refresh_token_must_be_cleared",
		NoRefresh:    false,
		Account:      "13800000000",
		AccountID:    12345,
		Region:       "cn",
		DeviceID:     "existing-device-id",
		ClientHeaders: map[string]string{
			"platform":    "ios",
			"app-version": "1.0.21",
		},
	}

	resp := &TokenResponse{
		OK:          true,
		Revision:    2,
		AccessToken: "fresh_broker_token_xyz",
		Source:      "loon:refresh",
		UpdatedAt:   "2026-11-20T12:00:00Z",
		ExpiresAt:   "2026-11-20T14:00:00Z",
		ETag:        `"rev-2-112233445566"`,
		// Empty optional fields must preserve existing config values!
		BaseURL:   "",
		Region:    "",
		Account:   "",
		AccountID: 0,
		DeviceID:  "",
		ClientHeaders: map[string]string{
			"phone-model":       "iPhone16,2",
			"xtcl-access-token": "evil",
			"authorization":     "evil",
			"cookie":            "evil",
			"platform":          "", // empty value must NOT overwrite existing "ios"
		},
	}

	if !ApplyToConfig(cfg, resp) {
		t.Fatalf("expected changed=true on first ApplyToConfig")
	}
	if cfg.Token != "fresh_broker_token_xyz" {
		t.Fatalf("unexpected token: %q", cfg.Token)
	}
	if cfg.RefreshToken != "" {
		t.Fatalf("expected RefreshToken cleared, got %q", cfg.RefreshToken)
	}
	if !cfg.NoRefresh || cfg.CanRefresh() {
		t.Fatalf("expected NoRefresh=true and CanRefresh=false")
	}
	if cfg.TokenSource != "broker:loon:refresh" {
		t.Fatalf("unexpected TokenSource: %q", cfg.TokenSource)
	}
	if cfg.TokenTime != "2026-11-20T12:00:00Z" || cfg.TokenExpiresAt != "2026-11-20T14:00:00Z" {
		t.Fatalf("unexpected timestamps: time=%q expires=%q", cfg.TokenTime, cfg.TokenExpiresAt)
	}
	if cfg.BrokerETag != `"rev-2-112233445566"` {
		t.Fatalf("unexpected ETag: %q", cfg.BrokerETag)
	}
	// Preserved metadata
	if cfg.BaseURL != "https://xr-api-p.rayneo.cn" || cfg.Region != "cn" ||
		cfg.Account != "13800000000" || cfg.AccountID != 12345 || cfg.DeviceID != "existing-device-id" {
		t.Fatalf("existing metadata was overwritten: %+v", cfg)
	}
	// Merged & filtered headers
	if cfg.ClientHeaders["platform"] != "ios" || cfg.ClientHeaders["app-version"] != "1.0.21" ||
		cfg.ClientHeaders["phone-model"] != "iPhone16,2" {
		t.Fatalf("expected merged safe headers, got %+v", cfg.ClientHeaders)
	}
	for _, forbidden := range []string{"xtcl-access-token", "authorization", "cookie"} {
		if _, exists := cfg.ClientHeaders[forbidden]; exists {
			t.Fatalf("forbidden header %q leaked into ClientHeaders", forbidden)
		}
	}

	// Applying the exact same response again should report changed=false.
	if ApplyToConfig(cfg, resp) {
		t.Fatalf("expected changed=false on idempotent ApplyToConfig")
	}
}

func TestParseExpiryAndFingerprint(t *testing.T) {
	if fp := TokenFingerprint("test_token"); len(fp) != 12 {
		t.Fatalf("expected 12-char hex fingerprint, got %q", fp)
	}
	if fp := TokenFingerprint("   "); fp != "" {
		t.Fatalf("expected empty fingerprint for blank token, got %q", fp)
	}

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	expired, rem, ok := ParseExpiry("2026-11-20T13:30:00Z", now)
	if !ok || expired || rem != 90*time.Minute {
		t.Fatalf("unexpected future expiry result: expired=%v rem=%v ok=%v", expired, rem, ok)
	}

	expired, rem, ok = ParseExpiry("2026-11-20T11:59:59Z", now)
	if !ok || !expired || rem != 0 {
		t.Fatalf("unexpected past expiry result: expired=%v rem=%v ok=%v", expired, rem, ok)
	}

	if _, _, ok := ParseExpiry("not-a-timestamp", now); ok {
		t.Fatalf("expected ok=false on invalid timestamp")
	}
}
