package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testSecret      = "test-broker-secret-0123456789abcdef"
	testReadSecret  = "test-read-only-secret-0123456789abcd"
	testWriteSecret = "test-write-only-secret-0123456789abc"
	testToken1      = "synthetic-access-token-alpha-001"
	testToken2      = "synthetic-access-token-beta-002"
)

func expectedFP(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func newTestBroker(t *testing.T, statePath, secret string, nowFn func() time.Time) (*Broker, *bytes.Buffer) {
	t.Helper()
	return newTestBrokerWithSecrets(t, statePath, SecretSet{Shared: secret}, nowFn)
}

func newTestBrokerWithSecrets(t *testing.T, statePath string, secrets SecretSet, nowFn func() time.Time) (*Broker, *bytes.Buffer) {
	t.Helper()
	store, err := NewStore(statePath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	b := NewBrokerWithSecrets(store, secrets, logger)
	if nowFn != nil {
		b.now = nowFn
	}
	return b, &logBuf
}

func doReq(t *testing.T, h http.Handler, method, path, auth string, body string, extraHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" && (extraHeaders == nil || extraHeaders["Content-Type"] == "") {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthNoAuthAndMethodCheck(t *testing.T) {
	// Even with empty secret, GET /health must succeed without auth (minimal fingerprint).
	b, _ := newTestBroker(t, "", "", nil)

	rec := doReq(t, b, http.MethodGet, "/health", "", "", map[string]string{"X-Forwarded-Proto": "https"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing security headers: %v", rec.Header())
	}
	if rec.Header().Get("Strict-Transport-Security") != "max-age=31536000; includeSubDomains" {
		t.Fatalf("missing HSTS header on HTTPS request: %q", rec.Header().Get("Strict-Transport-Security"))
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if got["ok"] != true || len(got) != 1 {
		t.Fatalf("unexpected health payload: %#v", got)
	}

	// Non-GET on /health -> 405
	recPost := doReq(t, b, http.MethodPost, "/health", "", "", nil)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health status = %d, want 405", recPost.Code)
	}
}

func TestRequireHTTPSMode(t *testing.T) {
	b, _ := newTestBroker(t, "", testSecret, nil)
	b.requireHTTPS = true

	recHTTP := doReq(t, b, http.MethodGet, "/v1/token", "Bearer "+testSecret, "", nil)
	if recHTTP.Code != http.StatusUpgradeRequired {
		t.Fatalf("HTTP request when requireHTTPS=true status = %d, want 426", recHTTP.Code)
	}

	recHTTPS := doReq(t, b, http.MethodGet, "/v1/token", "Bearer "+testSecret, "", map[string]string{"X-Forwarded-Proto": "https"})
	if recHTTPS.Code != http.StatusNotFound {
		t.Fatalf("HTTPS request when requireHTTPS=true status = %d, want 404", recHTTPS.Code)
	}
}

func TestFailClosedWhenSecretUnsetOrWeak(t *testing.T) {
	weakSecrets := []string{
		"",
		"   ",
		"replace-with-your-random-secret",
		"REPLACE-WITH-YOUR-RANDOM-SECRET-1234",
		"short-secret-123",
		strings.Repeat("a", 64),
		"abababababababababababababababab",
		"secret-with\nnewline-0123456789abcdef",
	}

	for _, sec := range weakSecrets {
		b, _ := newTestBroker(t, "", sec, nil)
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			rec := doReq(t, b, method, "/v1/token", "Bearer anything", `{"access_token":"tok"}`, nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("secret=%q %s /v1/token: status = %d, want 503", sec, method, rec.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid json error: %v", err)
			}
			if body["ok"] != false || body["error"] != "service unavailable" {
				t.Fatalf("expected generic 503 error, got %#v", body)
			}
		}
	}
}

func TestReadWriteSecretSeparationAndBackwardCompat(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	// Pure split secrets (no shared BROKER_SECRET)
	b, _ := newTestBrokerWithSecrets(t, "", SecretSet{
		Read:  testReadSecret,
		Write: testWriteSecret,
	}, func() time.Time { return now })

	if rec := doReq(t, b, http.MethodPost, "/v1/token", "Bearer "+testReadSecret, `{"access_token":"`+testToken1+`"}`, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read secret POST status = %d, want 403", rec.Code)
	}
	if rec := doReq(t, b, http.MethodDelete, "/v1/token", "Bearer "+testReadSecret, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("read secret DELETE status = %d, want 403", rec.Code)
	}
	if rec := doReq(t, b, http.MethodPost, "/v1/token", "Bearer "+testWriteSecret, `{"access_token":"`+testToken1+`"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("write secret POST status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, b, http.MethodGet, "/v1/token", "Bearer "+testWriteSecret, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("write secret GET status = %d, want 403", rec.Code)
	}
	if rec := doReq(t, b, http.MethodGet, "/v1/token", "Bearer "+testReadSecret, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("read secret GET status = %d, want 200", rec.Code)
	}
	if rec := doReq(t, b, http.MethodDelete, "/v1/token", "Bearer "+testWriteSecret, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("write secret DELETE status = %d, want 200", rec.Code)
	}

	// Hybrid migration: legacy shared secret + read-only secret
	bh, _ := newTestBrokerWithSecrets(t, "", SecretSet{
		Shared: testSecret,
		Read:   testReadSecret,
	}, func() time.Time { return now })
	if rec := doReq(t, bh, http.MethodPost, "/v1/token", "Bearer "+testSecret, `{"access_token":"`+testToken2+`"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("shared secret POST status = %d, want 200", rec.Code)
	}
	if rec := doReq(t, bh, http.MethodGet, "/v1/token", "Bearer "+testReadSecret, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("hybrid read secret GET status = %d, want 200", rec.Code)
	}
	if rec := doReq(t, bh, http.MethodDelete, "/v1/token", "Bearer "+testReadSecret, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("hybrid read secret DELETE status = %d, want 403", rec.Code)
	}
	if rec := doReq(t, bh, http.MethodDelete, "/v1/token", "Bearer "+testSecret, "", nil); rec.Code != http.StatusOK {
		t.Fatalf("hybrid shared secret DELETE status = %d, want 200", rec.Code)
	}
}

func TestAuthValidation(t *testing.T) {
	b, logBuf := newTestBroker(t, "", testSecret, nil)

	cases := []struct {
		name string
		auth string
	}{
		{"missing", ""},
		{"basic_scheme", "Basic dXNlcjpwYXNz"},
		{"bearer_empty", "Bearer "},
		{"wrong_short", "Bearer short"},
		{"wrong_same_len", "Bearer " + strings.Repeat("x", len(testSecret))},
		{"wrong_longer", "Bearer " + testSecret + "-extra"},
		{"no_scheme", testSecret},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, b, http.MethodGet, "/v1/token", tc.auth, "", nil)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal 401: %v", err)
			}
			if got["ok"] != false || got["error"] != "unauthorized" {
				t.Fatalf("unexpected 401 body: %#v", got)
			}
		})
	}

	// Case-insensitive Bearer scheme with exact secret passes auth (and hits 404 empty store).
	rec := doReq(t, b, http.MethodGet, "/v1/token", "bearer "+testSecret, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/token with lowercase bearer status = %d, want 404", rec.Code)
	}

	// Unknown route with valid auth -> 404
	recUnknown := doReq(t, b, http.MethodGet, "/v1/other", "Bearer "+testSecret, "", nil)
	if recUnknown.Code != http.StatusNotFound {
		t.Fatalf("GET /v1/other status = %d, want 404", recUnknown.Code)
	}

	// Unexpected query params on /v1/token -> 400
	recQuery := doReq(t, b, http.MethodGet, "/v1/token?secret=oops", "Bearer "+testSecret, "", nil)
	if recQuery.Code != http.StatusBadRequest {
		t.Fatalf("GET /v1/token?secret=oops status = %d, want 400", recQuery.Code)
	}

	// Unsupported method on /v1/token -> 405
	recPut := doReq(t, b, http.MethodPut, "/v1/token", "Bearer "+testSecret, "", nil)
	if recPut.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /v1/token status = %d, want 405", recPut.Code)
	}

	if strings.Contains(logBuf.String(), testSecret) {
		t.Fatalf("secret leaked into logs: %s", logBuf.String())
	}
}

func TestPostValidationAndBodyLimit(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b, _ := newTestBroker(t, "", testSecret, func() time.Time { return now })
	auth := "Bearer " + testSecret

	// Wrong Content-Type -> 415
	ctRec := doReq(t, b, http.MethodPost, "/v1/token", auth, `{"access_token":"abc"}`, map[string]string{"Content-Type": "text/plain"})
	if ctRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("wrong Content-Type status = %d, want 415", ctRec.Code)
	}

	// Missing Content-Type with JSON body -> 415
	noCtReq := httptest.NewRequest(http.MethodPost, "/v1/token", strings.NewReader(`{"access_token":"abc"}`))
	noCtReq.Header.Set("Authorization", auth)
	noCtRec := httptest.NewRecorder()
	b.ServeHTTP(noCtRec, noCtReq)
	if noCtRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("missing Content-Type status = %d, want 415", noCtRec.Code)
	}

	// Invalid UTF-8 bytes -> 400
	badUTF8Req := httptest.NewRequest(http.MethodPost, "/v1/token", bytes.NewReader([]byte{0x7b, 0xff, 0xfe, 0x7d}))
	badUTF8Req.Header.Set("Authorization", auth)
	badUTF8Req.Header.Set("Content-Type", "application/json")
	badUTF8Rec := httptest.NewRecorder()
	b.ServeHTTP(badUTF8Rec, badUTF8Req)
	if badUTF8Rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid UTF-8 status = %d, want 400", badUTF8Rec.Code)
	}

	badBodies := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"invalid_json", "{broken"},
		{"array_json", "[]"},
		{"trailing_json", `{"access_token":"abc"}{"extra":1}`},
		{"missing_token", `{"source":"manual"}`},
		{"whitespace_token", `{"access_token":"   "}`},
		{"space_in_token", `{"access_token":"abc def"}`},
		{"c1_in_token", "{\"access_token\":\"abc\u009bdef\"}"},
		{"too_long_token", fmt.Sprintf(`{"access_token":"%s"}`, strings.Repeat("a", 513))},
		{"newline_in_token", `{"access_token":"abc\ndef"}`},
		{"bad_captured_at", `{"access_token":"abc","captured_at":"not-a-timestamp"}`},
		{"year_out_of_range", `{"access_token":"abc","captured_at":"1999-12-31T23:59:59Z"}`},
		{"far_future_captured_at", `{"access_token":"abc","captured_at":"2027-01-01T00:00:00Z"}`},
		{"http_base_url", `{"access_token":"abc","base_url":"http://xr-api-p.rayneo.cn"}`},
		{"evil_base_url", `{"access_token":"abc","base_url":"https://evil.example.com"}`},
		{"localhost_base_url", `{"access_token":"abc","base_url":"http://127.0.0.1:8080"}`},
		{"port_base_url", `{"access_token":"abc","base_url":"https://xr-api-p.rayneo.cn:8443"}`},
		{"path_base_url", `{"access_token":"abc","base_url":"https://xr-api-p.rayneo.cn/extra"}`},
		{"userinfo_base_url", `{"access_token":"abc","base_url":"https://u:p@xr-api-p.rayneo.cn"}`},
		{"suffix_spoof_base_url", `{"access_token":"abc","base_url":"https://xr-api-p.rayneo.cn.evil.com"}`},
	}

	for _, tc := range badBodies {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, b, http.MethodPost, "/v1/token", auth, tc.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}

	// >64KB body -> 413 Request Entity Too Large
	huge := fmt.Sprintf(`{"access_token":"abc","account":"%s"}`, strings.Repeat("x", 70*1024))
	rec := doReq(t, b, http.MethodPost, "/v1/token", auth, huge, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRefreshTokenStrippedAndHeaderAllowlist(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	baseTime := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	b, logBuf := newTestBroker(t, statePath, testSecret, func() time.Time { return baseTime })
	auth := "Bearer " + testSecret

	// Include all 15 allowed headers (mixed case) AND forbidden headers AND both refresh_token / refreshToken.
	payload := `{
		"access_token": "  ` + testToken1 + `  ",
		"refresh_token": "SUPER-SECRET-REFRESH-1",
		"refreshToken": "SUPER-SECRET-REFRESH-2",
		"expires_in": 999999,
		"captured_at": "2026-11-20T19:59:30+08:00",
		"source": "loon:refresh",
		"base_url": "https://XR-API-P.RAYNEO.CN/",
		"region": "cn",
		"account": "synthetic-user",
		"account_id": 424242,
		"device_id": "synthetic-device-01",
		"client_headers": {
			"User-Agent": "RayNeoApp/3.0",
			"platform": "ios",
			"env": "prod",
			"locale": "zh_CN",
			"physicalRegion": "CN",
			"time-zone": "Asia/Shanghai",
			"app-version": "3.0.1",
			"version": "301",
			"app-build-number": "1001",
			"phone-os-version": "18.1",
			"phone-model": "iPhone16,2",
			"device-id": "dev-1",
			"deviceid": "dev-2",
			"device_id": "dev-3",
			"device-code": "dev-code-1",
			"xtcl-access-token": "MUST-DROP",
			"xtcl-authorization": "MUST-DROP",
			"xtcl-timestamp": "MUST-DROP",
			"Authorization": "Bearer MUST-DROP",
			"Host": "xr-api-p.rayneo.cn",
			"Content-Type": "application/json",
			"Content-Length": "123",
			"Accept-Encoding": "gzip",
			"Cookie": "sid=MUST-DROP",
			"x-arbitrary": "MUST-DROP"
		}
	}`

	rec := doReq(t, b, http.MethodPost, "/v1/token", auth, payload, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /v1/token status = %d, body=%s", rec.Code, rec.Body.String())
	}

	var postResp postTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &postResp); err != nil {
		t.Fatalf("unmarshal post resp: %v", err)
	}
	if !postResp.OK || !postResp.Updated || postResp.Revision != 1 {
		t.Fatalf("unexpected postResp: %+v", postResp)
	}
	if postResp.TokenFP != expectedFP(testToken1) {
		t.Fatalf("token_fp = %q, want %q", postResp.TokenFP, expectedFP(testToken1))
	}
	// expires_in capped at 86400s
	wantExp := baseTime.Add(86400 * time.Second).Format(time.RFC3339)
	if postResp.ExpiresAt != wantExp || postResp.ExpiresEstimated {
		t.Fatalf("expires_at=%q estimated=%v, want %q false", postResp.ExpiresAt, postResp.ExpiresEstimated, wantExp)
	}

	// Verify GET /v1/token
	getRec := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/token status = %d, body=%s", getRec.Code, getRec.Body.String())
	}
	rawGet := getRec.Body.String()
	if strings.Contains(rawGet, "refresh_token") || strings.Contains(rawGet, "refreshToken") || strings.Contains(rawGet, "SUPER-SECRET-REFRESH") {
		t.Fatalf("refresh token leaked in GET response: %s", rawGet)
	}
	if strings.Contains(rawGet, "MUST-DROP") {
		t.Fatalf("forbidden header leaked in GET response: %s", rawGet)
	}

	var getResp getTokenResponse
	if err := json.Unmarshal(getRec.Body.Bytes(), &getResp); err != nil {
		t.Fatalf("unmarshal get resp: %v", err)
	}
	if getResp.AccessToken != testToken1 {
		t.Fatalf("access_token = %q, want trimmed %q", getResp.AccessToken, testToken1)
	}
	if getResp.BaseURL != "https://xr-api-p.rayneo.cn" {
		t.Fatalf("base_url = %q, want normalized https://xr-api-p.rayneo.cn", getResp.BaseURL)
	}
	if len(getResp.ClientHeaders) != 15 {
		t.Fatalf("client_headers count = %d, want 15; got %#v", len(getResp.ClientHeaders), getResp.ClientHeaders)
	}

	// Verify persisted state file on disk has 0600 perms and zero refresh_token.
	fi, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat state file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("state file perms = %o, want 0600", fi.Mode().Perm())
	}
	diskBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	diskStr := string(diskBytes)
	if strings.Contains(diskStr, "refresh_token") || strings.Contains(diskStr, "refreshToken") || strings.Contains(diskStr, "SUPER-SECRET-REFRESH") {
		t.Fatalf("refresh token persisted to disk: %s", diskStr)
	}

	// Verify logs never contain raw access_token or BROKER_SECRET.
	logs := logBuf.String()
	if strings.Contains(logs, testToken1) || strings.Contains(logs, testSecret) || strings.Contains(logs, "SUPER-SECRET-REFRESH") {
		t.Fatalf("sensitive credential leaked in logs: %s", logs)
	}
}

func TestMetadataMergeTTLNonExtensionAntiDowngradeAndPurge(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	b, _ := newTestBroker(t, statePath, testSecret, func() time.Time { return now })
	auth := "Bearer " + testSecret

	// 1) Initial POST from loon:refresh with exact expires_in=7200
	r1 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"expires_in": 7200,
		"captured_at": "2026-11-20T10:00:00Z",
		"source": "loon:refresh",
		"base_url": "https://xr-api-p.rayneo.cn",
		"region": "cn",
		"account": "acct-one",
		"account_id": 1001,
		"client_headers": {"user-agent": "RayNeo/1.0"}
	}`, nil)
	if r1.Code != http.StatusOK {
		t.Fatalf("r1 status=%d body=%s", r1.Code, r1.Body.String())
	}
	var p1 postTokenResponse
	_ = json.Unmarshal(r1.Body.Bytes(), &p1)
	if !p1.Updated || p1.Revision != 1 || p1.ExpiresEstimated || p1.ExpiresAt != "2026-11-20T12:00:00Z" {
		t.Fatalf("unexpected p1: %+v", p1)
	}

	// 1b) Anti-downgrade: stale loon:header with older captured_at MUST NOT overwrite newer token
	rStale := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "old-in-flight-token",
		"captured_at": "2026-11-20T09:59:55Z",
		"source": "loon:header"
	}`, nil)
	var pStale postTokenResponse
	_ = json.Unmarshal(rStale.Body.Bytes(), &pStale)
	if pStale.Updated || pStale.Revision != 1 || pStale.TokenFP != expectedFP(testToken1) {
		t.Fatalf("stale header capture should not downgrade token: %+v", pStale)
	}

	// Advance clock by 30 minutes.
	now = now.Add(30 * time.Minute)

	// 2) Duplicate token without expires_in -> updated=false, rev=1, TTL NOT extended!
	r2 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"source": "loon:header"
	}`, nil)
	var p2 postTokenResponse
	_ = json.Unmarshal(r2.Body.Bytes(), &p2)
	if p2.Updated || p2.Revision != 1 || p2.ExpiresAt != "2026-11-20T12:00:00Z" || p2.ExpiresEstimated {
		t.Fatalf("duplicate token should not update or extend TTL: %+v", p2)
	}

	// 3) Duplicate token enriching client_headers + device_id -> rev=2, TTL NOT extended!
	r3 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"source": "loon:header",
		"device_id": "dev-enriched",
		"client_headers": {"phone-model": "iPhone16,2"}
	}`, nil)
	var p3 postTokenResponse
	_ = json.Unmarshal(r3.Body.Bytes(), &p3)
	if !p3.Updated || p3.Revision != 2 || p3.ExpiresAt != "2026-11-20T12:00:00Z" {
		t.Fatalf("metadata enrichment failed: %+v", p3)
	}

	// 4) Cross-account switch (1001 -> 2002) clears stale account, device_id, client_headers!
	rSwitch := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken2+`",
		"source": "loon:login",
		"expires_in": 3600,
		"account_id": 2002,
		"client_headers": {"platform": "android"}
	}`, nil)
	if rSwitch.Code != http.StatusOK {
		t.Fatalf("account switch status=%d body=%s", rSwitch.Code, rSwitch.Body.String())
	}
	gSwitch := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	var getSwitch getTokenResponse
	_ = json.Unmarshal(gSwitch.Body.Bytes(), &getSwitch)
	if getSwitch.AccountID != 2002 || getSwitch.Account != "" || getSwitch.DeviceID != "" || len(getSwitch.ClientHeaders) != 1 || getSwitch.ClientHeaders["platform"] != "android" {
		t.Fatalf("cross-account reset failed: %+v", getSwitch)
	}

	// 5) 5 min past expiry -> still returns 200 with expired=true (within 24h grace window)
	now = time.Date(2026, 11, 20, 11, 35, 0, 0, time.UTC)
	gExp := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	if gExp.Code != http.StatusOK {
		t.Fatalf("expired token within 24h grace GET status = %d, want 200", gExp.Code)
	}
	var getExp getTokenResponse
	_ = json.Unmarshal(gExp.Body.Bytes(), &getExp)
	if !getExp.Expired || getExp.RemainingSeconds != 0 {
		t.Fatalf("expected expired=true remaining=0, got %+v", getExp)
	}

	// 6) Past 24h post-expiry grace window -> auto-purged (GET returns 404 and file removed)!
	now = time.Date(2026, 11, 21, 11, 31, 0, 0, time.UTC)
	gPurged := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	if gPurged.Code != http.StatusNotFound {
		t.Fatalf("expired token past 24h grace GET status = %d, want 404", gPurged.Code)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected purged state file to be removed, stat err=%v", err)
	}
}

func TestETagAndDeleteAndRestartPersistence(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "nested", "state.json")
	now := time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)
	auth := "Bearer " + testSecret

	b1, _ := newTestBroker(t, statePath, testSecret, func() time.Time { return now })
	recPost := doReq(t, b1, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"expires_in": 3600,
		"source": "manual",
		"region": "cn"
	}`, nil)
	if recPost.Code != http.StatusOK {
		t.Fatalf("POST failed: %d %s", recPost.Code, recPost.Body.String())
	}

	wantETag := fmt.Sprintf(`"rev-1-%s"`, expectedFP(testToken1))

	// Restart: create a brand-new Store & Broker pointing at the same statePath.
	b2, _ := newTestBroker(t, statePath, testSecret, func() time.Time { return now.Add(10 * time.Second) })

	g := doReq(t, b2, http.MethodGet, "/v1/token", auth, "", nil)
	if g.Code != http.StatusOK {
		t.Fatalf("GET after restart status=%d, want 200", g.Code)
	}
	if etag := g.Header().Get("ETag"); etag != wantETag {
		t.Fatalf("ETag = %q, want %q", etag, wantETag)
	}

	// Matching If-None-Match variants -> 304 Not Modified
	for _, inm := range []string{
		wantETag,
		`W/` + wantETag,
		`"rev-0-000000000000", ` + wantETag,
		`"rev-0-000000000000", W/` + wantETag + `, "other"`,
		`*`,
	} {
		rec304 := doReq(t, b2, http.MethodGet, "/v1/token", auth, "", map[string]string{"If-None-Match": inm})
		if rec304.Code != http.StatusNotModified {
			t.Fatalf("If-None-Match=%q status=%d, want 304", inm, rec304.Code)
		}
		if rec304.Header().Get("ETag") != wantETag {
			t.Fatalf("304 ETag=%q, want %q", rec304.Header().Get("ETag"), wantETag)
		}
		if rec304.Body.Len() != 0 {
			t.Fatalf("304 should have empty body, got %q", rec304.Body.String())
		}
	}

	// DELETE /v1/token wipes memory and removes disk file.
	delRec := doReq(t, b2, http.MethodDelete, "/v1/token", auth, "", nil)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d, want 200", delRec.Code)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected state file to be removed after DELETE, stat err=%v", err)
	}

	// GET after DELETE -> 404
	if gAfter := doReq(t, b2, http.MethodGet, "/v1/token", auth, "", nil); gAfter.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status=%d, want 404", gAfter.Code)
	}

	// Second DELETE is idempotent -> 200
	if delRec2 := doReq(t, b2, http.MethodDelete, "/v1/token", auth, "", nil); delRec2.Code != http.StatusOK {
		t.Fatalf("second DELETE status=%d, want 200", delRec2.Code)
	}
}

func TestConcurrentAccess(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "concurrent.json")
	b, _ := newTestBroker(t, statePath, testSecret, nil)
	auth := "Bearer " + testSecret

	var wg sync.WaitGroup
	const workers = 12
	const iters = 25

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				switch (id + i) % 4 {
				case 0:
					body := fmt.Sprintf(`{"access_token":"tok-%d-%d","expires_in":3600,"source":"loon:refresh","client_headers":{"user-agent":"UA/%d"}}`, id, i, id)
					rec := doReq(t, b, http.MethodPost, "/v1/token", auth, body, nil)
					if rec.Code != http.StatusOK {
						t.Errorf("concurrent POST status=%d body=%s", rec.Code, rec.Body.String())
					}
				case 1:
					rec := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
					if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound {
						t.Errorf("concurrent GET status=%d", rec.Code)
					}
				case 2:
					rec := doReq(t, b, http.MethodGet, "/v1/token", auth, "", map[string]string{"If-None-Match": `"rev-1-000000000000"`})
					if rec.Code != http.StatusOK && rec.Code != http.StatusNotFound && rec.Code != http.StatusNotModified {
						t.Errorf("concurrent GET ETag status=%d", rec.Code)
					}
				case 3:
					if i%10 == 0 {
						rec := doReq(t, b, http.MethodDelete, "/v1/token", auth, "", nil)
						if rec.Code != http.StatusOK {
							t.Errorf("concurrent DELETE status=%d", rec.Code)
						}
					} else {
						rec := doReq(t, b, http.MethodGet, "/health", "", "", nil)
						if rec.Code != http.StatusOK {
							t.Errorf("concurrent health status=%d", rec.Code)
						}
					}
				}
			}
		}(w)
	}
	wg.Wait()
}
