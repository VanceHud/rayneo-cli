package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/VanceHud/rayneo-cli/internal/api"
	"github.com/VanceHud/rayneo-cli/internal/config"
)

func TestHARModeNeverCallsRefreshOn401(t *testing.T) {
	var refreshCalls int32
	var apiCalls int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authapi/token/refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"token":"should_never_happen"}}`))
			return
		}
		atomic.AddInt32(&apiCalls, 1)
		// Verify HAR client headers were mirrored
		if got := r.Header.Get("platform"); got != "ios" {
			t.Errorf("expected mirrored platform=ios, got %q", got)
		}
		if got := r.Header.Get("xtcl-access-token"); got != "har_captured_token_123456" {
			t.Errorf("expected har token, got %q", got)
		}
		_, _ = w.Write([]byte(`{"code":401,"msg":"Authentication failed"}`))
	}))
	defer srv.Close()

	harDoc := map[string]any{
		"log": map[string]any{
			"entries": []any{
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:00+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/lifelog/api/v1/count",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "har_captured_token_123456"},
							map[string]any{"name": "platform", "value": "ios"},
							map[string]any{"name": "accountid", "value": "100000000000001"},
							map[string]any{"name": "region", "value": "cn"},
						},
					},
					"response": map[string]any{
						"status":  200,
						"content": map[string]any{"text": `{"code":0,"data":{}}`},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(harDoc)
	tmpHar := filepath.Join(t.TempDir(), "capture.har")
	if err := os.WriteFile(tmpHar, b, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate an existing config that previously had a stale RefreshToken from SMS login.
	cfg := &config.Config{
		BaseURL:      srv.URL,
		Token:        "old_login_token",
		RefreshToken: "stale_refresh_token_that_would_kick_phone",
		Account:      "13800000000",
		AccountID:    100000000000001,
		Region:       "cn",
		DeviceID:     "00000000-0000-4000-8000-000000000001",
	}

	if _, err := applyHAR(cfg, tmpHar); err != nil {
		t.Fatalf("applyHAR: %v", err)
	}
	cfg.BaseURL = srv.URL // point to mock server

	if cfg.RefreshToken != "" {
		t.Fatalf("expected RefreshToken to be cleared, got %q", cfg.RefreshToken)
	}
	if !cfg.NoRefresh || cfg.CanRefresh() {
		t.Fatalf("expected NoRefresh=true and CanRefresh()=false")
	}
	if cfg.Account != "13800000000" {
		t.Fatalf("expected matching AccountID to preserve Account label, got %q", cfg.Account)
	}

	// Even if someone mutated RefreshToken back into memory, NoRefresh must still block refresh.
	cfg.RefreshToken = "forced_stale_rt"
	cl := api.New(cfg)
	_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/count", nil, nil)
	if err != nil {
		t.Fatalf("callWithAuth error: %v", err)
	}
	if !authExpired(body) {
		t.Fatalf("expected 401 body to be returned untouched")
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("refresh endpoint was called %d times; expected 0!", got)
	}
	if got := atomic.LoadInt32(&apiCalls); got != 1 {
		t.Fatalf("expected exactly 1 API call (no retry), got %d", got)
	}

	if err := doRefresh(cfg, false); err == nil {
		t.Fatalf("expected explicit doRefresh to refuse when NoRefresh is true")
	}
}

func TestVersionAndHelpSpec(t *testing.T) {
	if got := effectiveVersion(); got == "" {
		t.Fatalf("effectiveVersion returned empty string")
	}
	helpSpec.Version = effectiveVersion()
	b, err := json.Marshal(helpSpec)
	if err != nil {
		t.Fatalf("marshal helpSpec: %v", err)
	}
	if len(helpSpec.Commands) < 20 || len(b) == 0 {
		t.Fatalf("unexpected helpSpec command count: %d", len(helpSpec.Commands))
	}
}
