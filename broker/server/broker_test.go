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
	testSecret = "test-broker-secret-0123456789abcdef"
	testToken1 = "synthetic-access-token-alpha-001"
	testToken2 = "synthetic-access-token-beta-002"
)

func expectedFP(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func newTestBroker(t *testing.T, statePath, secret string, nowFn func() time.Time) (*Broker, *bytes.Buffer) {
	t.Helper()
	store, err := NewStore(statePath)
	if err != nil {
		t.Fatalf("NewStore failed: %v", err)
	}
	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	b := NewBroker(store, secret, logger)
	if nowFn != nil {
		b.now = nowFn
	}
	return b, &logBuf
}

func doReq(t *testing.T, h http.Handler, method, path, auth string, body string, extraHeaders map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if body != "" {
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
	// Even with empty secret, GET /health must succeed without auth.
	b, _ := newTestBroker(t, "", "", nil)

	rec := doReq(t, b, http.MethodGet, "/health", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal health: %v", err)
	}
	if got["ok"] != true || got["service"] != "rayneo-token-broker" || got["version"] != "1" {
		t.Fatalf("unexpected health payload: %#v", got)
	}

	// Non-GET on /health -> 405
	recPost := doReq(t, b, http.MethodPost, "/health", "", "", nil)
	if recPost.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /health status = %d, want 405", recPost.Code)
	}
}

func TestFailClosedWhenSecretUnset(t *testing.T) {
	b, _ := newTestBroker(t, "", "   ", nil)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		rec := doReq(t, b, method, "/v1/token", "Bearer anything", `{"access_token":"tok"}`, nil)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s /v1/token with empty secret: status = %d, want 503", method, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("invalid json error: %v", err)
		}
		if body["ok"] != false {
			t.Fatalf("expected ok=false, got %#v", body)
		}
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
	b, _ := newTestBroker(t, "", testSecret, nil)
	auth := "Bearer " + testSecret

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
		{"too_long_token", fmt.Sprintf(`{"access_token":"%s"}`, strings.Repeat("a", 513))},
		{"newline_in_token", `{"access_token":"abc\ndef"}`},
		{"bad_captured_at", `{"access_token":"abc","captured_at":"not-a-timestamp"}`},
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
		"base_url": "https://xr-api-p.rayneo.cn/",
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
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("missing Cache-Control: no-store")
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
	if getResp.CapturedAt != "2026-11-20T11:59:30Z" {
		t.Fatalf("captured_at = %q, want 2026-11-20T11:59:30Z", getResp.CapturedAt)
	}
	if getResp.BaseURL != "https://xr-api-p.rayneo.cn" {
		t.Fatalf("base_url = %q, want trimmed trailing slash", getResp.BaseURL)
	}
	if len(getResp.ClientHeaders) != 15 {
		t.Fatalf("client_headers count = %d, want 15; got %#v", len(getResp.ClientHeaders), getResp.ClientHeaders)
	}
	if getResp.ClientHeaders["user-agent"] != "RayNeoApp/3.0" || getResp.ClientHeaders["physicalregion"] != "CN" {
		t.Fatalf("unexpected header normalization: %#v", getResp.ClientHeaders)
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

func TestMetadataMergeTTLNonExtensionAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	b, _ := newTestBroker(t, "", testSecret, func() time.Time { return now })
	auth := "Bearer " + testSecret

	// 1) Initial POST from loon:refresh with exact expires_in=7200 and minimal metadata.
	r1 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"expires_in": 7200,
		"source": "loon:refresh",
		"base_url": "https://xr-api-p.rayneo.cn",
		"region": "cn",
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

	// Advance clock by 30 minutes.
	now = now.Add(30 * time.Minute)

	// 2) Duplicate token from loon:header with NO expires_in and NO new metadata -> updated=false, rev=1, TTL NOT extended!
	r2 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"source": "loon:header"
	}`, nil)
	var p2 postTokenResponse
	_ = json.Unmarshal(r2.Body.Bytes(), &p2)
	if p2.Updated || p2.Revision != 1 || p2.ExpiresAt != "2026-11-20T12:00:00Z" || p2.ExpiresEstimated {
		t.Fatalf("duplicate token should not update or extend TTL: %+v", p2)
	}

	// 3) Duplicate token from loon:header enriching client_headers + device_id (still NO expires_in):
	//    Must merge headers (keep user-agent), keep base_url/region/account_id, keep expires_at (NOT extended!), keep source=loon:refresh, bump rev=2!
	r3 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken1+`",
		"source": "loon:header",
		"device_id": "dev-enriched",
		"client_headers": {"phone-model": "iPhone16,2"}
	}`, nil)
	var p3 postTokenResponse
	_ = json.Unmarshal(r3.Body.Bytes(), &p3)
	if !p3.Updated || p3.Revision != 2 || p3.ExpiresAt != "2026-11-20T12:00:00Z" || p3.ExpiresEstimated {
		t.Fatalf("metadata enrichment should bump rev and keep expires_at: %+v", p3)
	}

	g3 := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	var get3 getTokenResponse
	_ = json.Unmarshal(g3.Body.Bytes(), &get3)
	if get3.Source != "loon:refresh" || get3.BaseURL != "https://xr-api-p.rayneo.cn" || get3.Region != "cn" || get3.AccountID != 1001 || get3.DeviceID != "dev-enriched" {
		t.Fatalf("metadata preservation failed: %+v", get3)
	}
	if get3.ClientHeaders["user-agent"] != "RayNeo/1.0" || get3.ClientHeaders["phone-model"] != "iPhone16,2" {
		t.Fatalf("client_headers merge failed: %#v", get3.ClientHeaders)
	}
	if get3.Expired || get3.RemainingSeconds != 5400 {
		t.Fatalf("expected expired=false remaining=5400, got expired=%v remaining=%d", get3.Expired, get3.RemainingSeconds)
	}

	// 4) Advance clock past expiry (+2 hours from initial -> now = 12:05:00Z).
	//    GET must STILL return 200 with expired=true, remaining_seconds=0.
	now = time.Date(2026, 11, 20, 12, 5, 0, 0, time.UTC)
	gExp := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	if gExp.Code != http.StatusOK {
		t.Fatalf("expired token GET status = %d, want 200", gExp.Code)
	}
	var getExp getTokenResponse
	_ = json.Unmarshal(gExp.Body.Bytes(), &getExp)
	if !getExp.Expired || getExp.RemainingSeconds != 0 {
		t.Fatalf("expected expired=true remaining=0, got %+v", getExp)
	}

	// 5) New token WITHOUT expires_in -> estimated TTL = now + 7200s, preserves prior metadata!
	r4 := doReq(t, b, http.MethodPost, "/v1/token", auth, `{
		"access_token": "`+testToken2+`",
		"source": "loon:header"
	}`, nil)
	var p4 postTokenResponse
	_ = json.Unmarshal(r4.Body.Bytes(), &p4)
	if !p4.Updated || p4.Revision != 3 || !p4.ExpiresEstimated || p4.ExpiresAt != "2026-11-20T14:05:00Z" {
		t.Fatalf("new token without expires_in should estimate +7200s: %+v", p4)
	}

	g4 := doReq(t, b, http.MethodGet, "/v1/token", auth, "", nil)
	var get4 getTokenResponse
	_ = json.Unmarshal(g4.Body.Bytes(), &get4)
	if get4.BaseURL != "https://xr-api-p.rayneo.cn" || get4.ClientHeaders["user-agent"] != "RayNeo/1.0" || get4.ClientHeaders["phone-model"] != "iPhone16,2" {
		t.Fatalf("new token should preserve previous metadata when omitted: %+v", get4)
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
		if rec304.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("304 missing Cache-Control: no-store")
		}
		if rec304.Body.Len() != 0 {
			t.Fatalf("304 should have empty body, got %q", rec304.Body.String())
		}
	}

	// Non-matching If-None-Match -> 200
	rec200 := doReq(t, b2, http.MethodGet, "/v1/token", auth, "", map[string]string{"If-None-Match": `"rev-99-abcdef012345"`})
	if rec200.Code != http.StatusOK {
		t.Fatalf("mismatched ETag status=%d, want 200", rec200.Code)
	}

	// DELETE /v1/token wipes memory and removes disk file.
	delRec := doReq(t, b2, http.MethodDelete, "/v1/token", auth, "", nil)
	if delRec.Code != http.StatusOK {
		t.Fatalf("DELETE status=%d, want 200", delRec.Code)
	}
	var delResp map[string]any
	_ = json.Unmarshal(delRec.Body.Bytes(), &delResp)
	if delResp["ok"] != true || delResp["cleared"] != true {
		t.Fatalf("unexpected DELETE resp: %#v", delResp)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("expected state file to be removed after DELETE, stat err=%v", err)
	}

	// GET after DELETE -> 404
	gAfter := doReq(t, b2, http.MethodGet, "/v1/token", auth, "", nil)
	if gAfter.Code != http.StatusNotFound {
		t.Fatalf("GET after DELETE status=%d, want 404", gAfter.Code)
	}

	// Second DELETE is idempotent -> 200
	delRec2 := doReq(t, b2, http.MethodDelete, "/v1/token", auth, "", nil)
	if delRec2.Code != http.StatusOK {
		t.Fatalf("second DELETE status=%d, want 200", delRec2.Code)
	}

	// Restart after DELETE stays empty -> 404
	b3, _ := newTestBroker(t, statePath, testSecret, nil)
	gRestart := doReq(t, b3, http.MethodGet, "/v1/token", auth, "", nil)
	if gRestart.Code != http.StatusNotFound {
		t.Fatalf("GET after restart post-DELETE status=%d, want 404", gRestart.Code)
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
