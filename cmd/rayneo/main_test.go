package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
	"github.com/VanceHud/rayneo-cli/internal/broker"
	"github.com/VanceHud/rayneo-cli/internal/config"
)

func resetGlobals(t *testing.T) {
	t.Helper()
	origHar := harPath
	origNoRef := noRefreshFlag
	origBURL := brokerURLFlag
	origBSec := brokerSecretFlag
	harPath = ""
	noRefreshFlag = false
	brokerURLFlag = ""
	brokerSecretFlag = ""
	t.Setenv("RAYNEO_BROKER_URL", "")
	t.Setenv("RAYNEO_BROKER_SECRET", "")
	t.Setenv("RAYNEO_TOKEN", "")
	t.Setenv("RAYNEO_HAR", "")
	t.Setenv("RAYNEO_NO_REFRESH", "")
	t.Cleanup(func() {
		harPath = origHar
		noRefreshFlag = origNoRef
		brokerURLFlag = origBURL
		brokerSecretFlag = origBSec
	})
}

func TestHARModeNeverCallsRefreshOn401(t *testing.T) {
	resetGlobals(t)
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

func TestSyncCommandLifecycle(t *testing.T) {
	resetGlobals(t)
	tmpCfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmpCfg)

	const secret = "test-broker-secret-xyz-987654321"
	const tok1 = "synced_access_token_rev1_abcdef"

	var verifyCalls int32
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profileapi/v1/profile/query" {
			t.Errorf("unexpected verify path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&verifyCalls, 1)
		if got := r.Header.Get("xtcl-access-token"); got != tok1 {
			t.Errorf("verify received wrong token: %q", got)
			_, _ = w.Write([]byte(`{"code":401,"msg":"Authentication failed"}`))
			return
		}
		if got := r.Header.Get("platform"); got != "ios" {
			t.Errorf("verify expected mirrored platform=ios, got %q", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"data":{"nickName":"tester"}}`))
	}))
	defer apiSrv.Close()

	var (
		mu         sync.Mutex
		hasToken   = false
		curRev     = int64(1)
		curTok     = tok1
		lastINM    = ""
		brokerHits int32
	)
	expiresAt := time.Now().UTC().Add(90 * time.Minute).Format(time.RFC3339)

	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&brokerHits, 1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"ok":false,"error":"unauthorized"}`))
			return
		}
		mu.Lock()
		defer mu.Unlock()
		lastINM = r.Header.Get("If-None-Match")
		if !hasToken {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"ok":false,"error":"no token synced yet"}`))
			return
		}
		fp := broker.TokenFingerprint(curTok)
		etag := `"rev-1-` + fp + `"`
		if lastINM == etag {
			w.Header().Set("ETag", etag)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"revision":          curRev,
			"access_token":      curTok,
			"token_fp":          fp,
			"source":            "loon:refresh",
			"captured_at":       time.Now().UTC().Format(time.RFC3339),
			"updated_at":        time.Now().UTC().Format(time.RFC3339),
			"expires_at":        expiresAt,
			"expires_estimated": false,
			"expired":           false,
			"remaining_seconds": 5400,
			"base_url":          apiSrv.URL,
			"region":            "cn",
			"account_id":        54321,
			"device_id":         "device-from-broker",
			"client_headers": map[string]string{
				"platform":          "ios",
				"xtcl-access-token": "must_be_dropped",
			},
		})
	}))
	defer brokerSrv.Close()

	// Seed pre-existing config with a stale refresh token to confirm sync wipes it.
	seed, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	seed.RefreshToken = "stale_rt_should_disappear"
	seed.NoRefresh = false
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	// 1) Wrong secret (401) -> fails without saving bad secret.
	var out, errBuf bytes.Buffer
	if code := runSync([]string{"-url", brokerSrv.URL, "-secret", "wrong-secret"}, false, &out, &errBuf); code != 1 {
		t.Fatalf("expected exit 1 on 401, got %d (out=%q err=%q)", code, out.String(), errBuf.String())
	}
	cfgAfter401, _ := config.Load()
	if cfgAfter401.BrokerSecret != "" {
		t.Fatalf("expected failed 401 sync not to save secret, got %q", cfgAfter401.BrokerSecret)
	}

	// 2) Valid secret while broker has no token yet (404) -> saves broker config, exits 1 with hint.
	out.Reset()
	errBuf.Reset()
	if code := runSync([]string{"-url", brokerSrv.URL, "-secret", secret}, false, &out, &errBuf); code != 1 {
		t.Fatalf("expected exit 1 on 404, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "RayNeo App") {
		t.Fatalf("expected actionable RayNeo App message on 404, got %q", errBuf.String())
	}
	if strings.Contains(out.String()+errBuf.String(), secret) {
		t.Fatalf("raw broker secret leaked on 404!")
	}

	// 3) Broker now has token -> sync -verify succeeds and verifies against /profileapi/v1/profile/query.
	mu.Lock()
	hasToken = true
	mu.Unlock()

	out.Reset()
	errBuf.Reset()
	if code := runSync([]string{"-verify"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("expected exit 0 on sync -verify, got %d (out=%q err=%q)", code, out.String(), errBuf.String())
	}
	if atomic.LoadInt32(&verifyCalls) != 1 {
		t.Fatalf("expected 1 verify call to /profileapi/v1/profile/query, got %d", verifyCalls)
	}
	if !strings.Contains(out.String(), "verified : HTTP 200 OK") {
		t.Fatalf("expected verified output, got %q", out.String())
	}
	if strings.Contains(out.String()+errBuf.String(), secret) {
		t.Fatalf("raw broker secret leaked in sync output!")
	}

	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Token != tok1 || !saved.NoRefresh || saved.RefreshToken != "" || saved.AccountID != 54321 {
		t.Fatalf("unexpected saved config after sync: %+v", saved)
	}
	if saved.BrokerETag == "" || saved.TokenExpiresAt != expiresAt {
		t.Fatalf("expected BrokerETag and TokenExpiresAt saved, got etag=%q exp=%q", saved.BrokerETag, saved.TokenExpiresAt)
	}
	if saved.ClientHeaders["platform"] != "ios" {
		t.Fatalf("expected platform=ios saved, got %+v", saved.ClientHeaders)
	}
	if _, leaked := saved.ClientHeaders["xtcl-access-token"]; leaked {
		t.Fatalf("unsafe xtcl-access-token header leaked into saved config")
	}

	// 4) Second sync uses ETag -> 304 Not Modified.
	out.Reset()
	errBuf.Reset()
	if code := runSync(nil, false, &out, &errBuf); code != 0 {
		t.Fatalf("expected exit 0 on 304 sync, got %d (err=%q)", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "304 Not Modified") {
		t.Fatalf("expected 304 Not Modified output, got %q", out.String())
	}

	// 5) sync -force skips If-None-Match.
	out.Reset()
	errBuf.Reset()
	if code := runSync([]string{"-force"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("expected exit 0 on sync -force, got %d (err=%q)", code, errBuf.String())
	}
	mu.Lock()
	gotINM := lastINM
	mu.Unlock()
	if gotINM != "" {
		t.Fatalf("expected empty If-None-Match on -force, got %q", gotINM)
	}
	if !strings.Contains(out.String(), "synced from") {
		t.Fatalf("expected full sync output on -force, got %q", out.String())
	}

	// 6) sync -status prints broker URL, masked secret, token fingerprint, expiry, never raw secret.
	out.Reset()
	errBuf.Reset()
	if code := runSync([]string{"-status"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("expected exit 0 on sync -status, got %d", code)
	}
	statusOut := out.String()
	if strings.Contains(statusOut, secret) {
		t.Fatalf("raw broker secret leaked in sync -status: %q", statusOut)
	}
	if !strings.Contains(statusOut, brokerSrv.URL) ||
		!strings.Contains(statusOut, broker.TokenFingerprint(tok1)) ||
		!strings.Contains(statusOut, expiresAt) ||
		!strings.Contains(statusOut, "no (remaining") {
		t.Fatalf("unexpected sync -status output:\n%s", statusOut)
	}

	// 7) sync -clear removes broker_url, broker_secret, broker_etag.
	out.Reset()
	errBuf.Reset()
	if code := runSync([]string{"-clear"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("expected exit 0 on sync -clear, got %d", code)
	}
	cleared, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cleared.BrokerURL != "" || cleared.BrokerSecret != "" || cleared.BrokerETag != "" {
		t.Fatalf("expected broker fields cleared, got url=%q secret=%q etag=%q",
			cleared.BrokerURL, cleared.BrokerSecret, cleared.BrokerETag)
	}
}

func TestConfigMasksBrokerSecret(t *testing.T) {
	resetGlobals(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const rawSecret = "my-ultra-sensitive-broker-secret-999"
	var out, errBuf bytes.Buffer
	if code := runConfig([]string{"-broker-url", "https://broker.example.com/", "-broker-secret", rawSecret}, &out, &errBuf); code != 0 {
		t.Fatalf("runConfig failed with code %d: %s", code, errBuf.String())
	}
	if strings.Contains(out.String(), rawSecret) {
		t.Fatalf("cmdConfig leaked raw broker secret in output: %q", out.String())
	}
	if !strings.Contains(out.String(), "broker=https://broker.example.com") || !strings.Contains(out.String(), "brokerSecret=") {
		t.Fatalf("unexpected cmdConfig output: %q", out.String())
	}
}

func TestBrokerPreSyncAnd401AutoRecovery(t *testing.T) {
	resetGlobals(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const secret = "recovery-broker-secret-0123456789"
	const staleTok = "expired_token_111111111111"
	const freshTok = "fresh_token_222222222222"

	var (
		mu            sync.Mutex
		brokerTok     = freshTok
		brokerRev     = int64(2)
		brokerCalls   int32
		lastBrokerINM string
	)
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&brokerCalls, 1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		tok := brokerTok
		rev := brokerRev
		lastBrokerINM = r.Header.Get("If-None-Match")
		mu.Unlock()

		fp := broker.TokenFingerprint(tok)
		w.Header().Set("ETag", `"rev-2-`+fp+`"`)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":                true,
			"revision":          rev,
			"access_token":      tok,
			"token_fp":          fp,
			"source":            "loon:refresh",
			"updated_at":        time.Now().UTC().Format(time.RFC3339),
			"expires_at":        time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339),
			"remaining_seconds": 7200,
			"client_headers":    map[string]string{"platform": "ios"},
		})
	}))
	defer brokerSrv.Close()

	var (
		apiCalls     int32
		refreshCalls int32
	)
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authapi/token/refresh" {
			atomic.AddInt32(&refreshCalls, 1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"token":"must_never_be_called"}}`))
			return
		}
		atomic.AddInt32(&apiCalls, 1)
		if r.Header.Get("xtcl-access-token") == freshTok {
			_, _ = w.Write([]byte(`{"code":0,"data":{"ok":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":401,"msg":"Authentication failed"}`))
	}))
	defer apiSrv.Close()

	// Case 1: 401 auto-recovery fetches fresh token without ETag, saves config, and retries once!
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.BaseURL = apiSrv.URL
	cfg.Token = staleTok
	cfg.RefreshToken = "legacy_rt_must_never_run"
	cfg.NoRefresh = true
	cfg.BrokerURL = brokerSrv.URL
	cfg.BrokerSecret = secret
	cfg.BrokerETag = `"rev-1-old"`
	cfg.TokenExpiresAt = time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339) // not yet expired locally, but rejected by server
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	cl := api.New(cfg)
	_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/count", nil, nil)
	if err != nil {
		t.Fatalf("callWithAuth error: %v", err)
	}
	if authExpired(body) || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("expected transparent 401 recovery to succeed, got body=%s", string(body))
	}
	if got := atomic.LoadInt32(&apiCalls); got != 2 {
		t.Fatalf("expected 2 API calls (initial 401 + 1 retry), got %d", got)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("refresh endpoint called %d times; expected 0!", got)
	}
	mu.Lock()
	inm := lastBrokerINM
	mu.Unlock()
	if inm != "" {
		t.Fatalf("expected 401 recovery to force-fetch without If-None-Match, got %q", inm)
	}
	reloaded, _ := config.Load()
	if reloaded.Token != freshTok || reloaded.RefreshToken != "" || !reloaded.NoRefresh {
		t.Fatalf("expected recovered token saved with no_refresh=true & empty refresh_token, got %+v", reloaded)
	}

	// Case 2: 401 when broker returns the SAME token -> no retry, no /authapi/token/refresh.
	atomic.StoreInt32(&apiCalls, 0)
	mu.Lock()
	brokerTok = staleTok
	mu.Unlock()
	cfg.Token = staleTok
	_, body, err = callWithAuth(cl, "GET", "/lifelog/api/v1/count", nil, nil)
	if err != nil {
		t.Fatalf("callWithAuth error: %v", err)
	}
	if !authExpired(body) {
		t.Fatalf("expected 401 body when broker has same token, got %s", string(body))
	}
	if got := atomic.LoadInt32(&apiCalls); got != 1 {
		t.Fatalf("expected 1 API call (no retry when token unchanged), got %d", got)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("refresh endpoint called %d times; expected 0!", got)
	}

	// Case 3: Pre-request auto-sync in mustConfig() when TokenExpiresAt is in the past.
	mu.Lock()
	brokerTok = freshTok
	mu.Unlock()
	cfg.Token = staleTok
	cfg.TokenExpiresAt = time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	cfg.BrokerETag = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	synced := mustConfig()
	if synced.Token != freshTok {
		t.Fatalf("expected mustConfig pre-sync to pull fresh token %q, got %q", freshTok, synced.Token)
	}

	// Case 3b: Pre-request auto-sync in mustConfig() when Token is empty.
	cfg.Token = ""
	cfg.TokenExpiresAt = ""
	cfg.BrokerETag = ""
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	syncedEmpty := mustConfig()
	if syncedEmpty.Token != freshTok || !syncedEmpty.NoRefresh || syncedEmpty.RefreshToken != "" {
		t.Fatalf("expected mustConfig pre-sync on empty token to pull %q with no_refresh=true, got %+v", freshTok, syncedEmpty)
	}

	// Case 3c: doRefresh must refuse when broker is configured even if NoRefresh were cleared.
	syncedEmpty.NoRefresh = false
	syncedEmpty.RefreshToken = "forced_rt"
	if err := doRefresh(syncedEmpty, false); err == nil {
		t.Fatalf("expected doRefresh to refuse when broker is configured")
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("refresh endpoint called %d times; expected 0!", got)
	}

	// Case 4: When -har is active, broker auto-sync is strictly disabled on 401.
	atomic.StoreInt32(&brokerCalls, 0)
	atomic.StoreInt32(&apiCalls, 0)
	harPath = "/tmp/dummy-active.har"
	cfg.Token = staleTok
	_, body, err = callWithAuth(cl, "GET", "/lifelog/api/v1/count", nil, nil)
	if err != nil {
		t.Fatalf("callWithAuth error: %v", err)
	}
	if !authExpired(body) {
		t.Fatalf("expected 401 in -har mode")
	}
	if got := atomic.LoadInt32(&brokerCalls); got != 0 {
		t.Fatalf("expected 0 broker calls when -har is active, got %d", got)
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 0 {
		t.Fatalf("expected 0 refresh calls when -har is active, got %d", got)
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
	if len(helpSpec.Commands) != 31 || len(b) == 0 {
		t.Fatalf("unexpected helpSpec command count: %d", len(helpSpec.Commands))
	}
	wantCmds := map[string]bool{
		"sync": false, "trail": false, "digest": false,
		"facts": false, "devices": false, "profile": false, "quota": false, "tasks": false,
	}
	for _, c := range helpSpec.Commands {
		if _, ok := wantCmds[c.Name]; ok {
			wantCmds[c.Name] = true
		}
		if !c.ReadOnly && !c.RequiresConfirmation {
			t.Errorf("non-read-only command %q must set RequiresConfirmation=true", c.Name)
		}
	}
	if helpSpec.Safety == "" {
		t.Fatalf("expected non-empty helpSpec.Safety permission rule")
	}
	for name, seen := range wantCmds {
		if !seen {
			t.Fatalf("expected %q subcommand in helpSpec", name)
		}
	}
}
