// Package broker implements the Scheme A (Passive Token Broker) HTTP client.
// The broker mirrors only the active read-only access_token and safe client
// metadata captured passively from the mobile App; it never handles refresh
// tokens so the mobile App remains the sole authority for token renewal.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/config"
)

const (
	maxResponseBytes = 256 * 1024
	maxTokenLen      = 512
	maxHeaderValLen  = 1024
)

var rayNeoAPIHostRe = regexp.MustCompile(`(?i)^xr-api-[a-z0-9-]+\.rayneo\.(cn|com)$`)

// Sentinel errors returned by Fetch.
var (
	ErrNoToken      = errors.New("broker has no token synced yet")
	ErrUnauthorized = errors.New("broker authentication failed (401 unauthorized)")
	ErrForbidden    = errors.New("broker access forbidden (403: secret lacks read permission)")
)

// allowedClientHeaders mirrors internal/har/har.go's exact 15-header allowlist
// of non-signed device/app headers safe to replay. Never include xtcl-*,
// authorization, host, content-*, accept-encoding, or cookie.
var allowedClientHeaders = map[string]bool{
	"user-agent":       true,
	"platform":         true,
	"env":              true,
	"locale":           true,
	"physicalregion":   true,
	"time-zone":        true,
	"app-version":      true,
	"version":          true,
	"app-build-number": true,
	"phone-os-version": true,
	"phone-model":      true,
	"device-id":        true,
	"deviceid":         true,
	"device_id":        true,
	"device-code":      true,
}

// TokenResponse represents the V1 GET /v1/token JSON payload plus HTTP ETag.
// RefreshToken is intentionally absent and never accepted, stored, or returned.
type TokenResponse struct {
	OK               bool              `json:"ok"`
	Revision         int64             `json:"revision"`
	AccessToken      string            `json:"access_token"`
	TokenFP          string            `json:"token_fp"`
	Source           string            `json:"source"`
	CapturedAt       string            `json:"captured_at"`
	UpdatedAt        string            `json:"updated_at"`
	ExpiresAt        string            `json:"expires_at"`
	ExpiresEstimated bool              `json:"expires_estimated"`
	Expired          bool              `json:"expired"`
	RemainingSeconds int64             `json:"remaining_seconds"`
	BaseURL          string            `json:"base_url"`
	Region           string            `json:"region"`
	Account          string            `json:"account"`
	AccountID        int64             `json:"account_id"`
	DeviceID         string            `json:"device_id"`
	ClientHeaders    map[string]string `json:"client_headers"`
	ETag             string            `json:"-"`
}

// Client fetches token state from a RayNeo Token Broker.
type Client struct {
	HTTP *http.Client
}

// DefaultClient is used by package-level Fetch.
var DefaultClient = &Client{
	HTTP: &http.Client{Timeout: 10 * time.Second},
}

// ValidateRayNeoBaseURL verifies that a non-empty base_url from a broker points
// strictly to an official RayNeo HTTPS API gateway origin (https://xr-api-*.rayneo.(cn|com)),
// preventing compromised or misconfigured brokers from redirecting CLI credentials
// or triggering local SSRF.
func ValidateRayNeoBaseURL(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", true
	}
	if len(trimmed) > 256 || hasControlChars(trimmed) {
		return "", false
	}
	trimmed = strings.TrimRight(trimmed, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(u.Scheme, "https") ||
		u.User != nil ||
		u.Port() != "" ||
		(u.Path != "" && u.Path != "/") ||
		u.RawQuery != "" ||
		u.Fragment != "" ||
		!rayNeoAPIHostRe.MatchString(u.Hostname()) {
		return "", false
	}
	return "https://" + strings.ToLower(u.Hostname()), true
}

// Fetch retrieves the latest token from <brokerURL>/v1/token using DefaultClient.
// When ifNoneMatch matches the broker's current ETag, Fetch returns (nil, true, nil).
func Fetch(ctx context.Context, brokerURL, secret, ifNoneMatch string) (*TokenResponse, bool, error) {
	return DefaultClient.Fetch(ctx, brokerURL, secret, ifNoneMatch)
}

// Fetch retrieves the latest token from <brokerURL>/v1/token.
func (c *Client) Fetch(ctx context.Context, brokerURL, secret, ifNoneMatch string) (*TokenResponse, bool, error) {
	base := strings.TrimRight(strings.TrimSpace(brokerURL), "/")
	sec := strings.TrimSpace(secret)
	if base == "" {
		return nil, false, errors.New("broker URL is empty")
	}
	if sec == "" {
		return nil, false, errors.New("broker secret is empty")
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false, errors.New("broker URL must be a valid http(s) URL")
	}

	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := base + "/v1/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build broker request: %s", sanitizeText(err.Error(), sec))
	}
	req.Header.Set("Authorization", "Bearer "+sec)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")
	if inm := strings.TrimSpace(ifNoneMatch); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}

	hc := c.HTTP
	if hc == nil {
		hc = DefaultClient.HTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("broker request failed: %s", sanitizeText(err.Error(), sec))
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, true, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read broker response: %s", sanitizeText(err.Error(), sec))
	}
	if len(body) > maxResponseBytes {
		return nil, false, errors.New("broker response too large")
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return nil, false, ErrUnauthorized
	case http.StatusForbidden:
		return nil, false, ErrForbidden
	case http.StatusNotFound:
		return nil, false, ErrNoToken
	case http.StatusOK:
		// handled below
	default:
		msg := extractBrokerError(body)
		if msg != "" {
			return nil, false, fmt.Errorf("broker HTTP %d: %s", resp.StatusCode, sanitizeText(msg, sec))
		}
		return nil, false, fmt.Errorf("broker HTTP %d", resp.StatusCode)
	}

	var envelope struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, false, fmt.Errorf("broker returned invalid JSON: %s", sanitizeText(err.Error(), sec))
	}
	if envelope.OK != nil && !*envelope.OK {
		msg := strings.TrimSpace(envelope.Error)
		if msg == "" {
			msg = "ok=false"
		}
		return nil, false, fmt.Errorf("broker error: %s", sanitizeText(msg, sec))
	}

	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, false, fmt.Errorf("broker returned invalid token payload: %s", sanitizeText(err.Error(), sec))
	}
	tr.AccessToken = strings.TrimSpace(tr.AccessToken)
	if tr.AccessToken == "" || len(tr.AccessToken) > maxTokenLen || !isPrintableASCIINoSpace(tr.AccessToken) {
		return nil, false, errors.New("broker returned invalid access_token")
	}
	if tr.BaseURL != "" {
		validBase, ok := ValidateRayNeoBaseURL(tr.BaseURL)
		if !ok {
			return nil, false, errors.New("broker returned untrusted base_url")
		}
		tr.BaseURL = validBase
	}
	tr.ETag = strings.TrimSpace(resp.Header.Get("ETag"))
	if tr.TokenFP == "" {
		tr.TokenFP = TokenFingerprint(tr.AccessToken)
	}
	tr.ClientHeaders = FilterClientHeaders(tr.ClientHeaders)
	return &tr, false, nil
}

// ApplyToConfig updates cfg with a fetched broker TokenResponse, enforcing
// NoRefresh=true and clearing RefreshToken so the CLI never calls
// /authapi/token/refresh. Optional metadata and allowlisted client headers are
// merged without overwriting existing non-empty values with empty ones.
func ApplyToConfig(cfg *config.Config, resp *TokenResponse) (changed bool) {
	if cfg == nil || resp == nil {
		return false
	}

	tok := strings.TrimSpace(resp.AccessToken)
	tokenChanged := false
	if tok != "" && len(tok) <= maxTokenLen && isPrintableASCIINoSpace(tok) && cfg.Token != tok {
		cfg.Token = tok
		tokenChanged = true
		changed = true
	}

	// Enforce anti-kickout invariants: broker mode must NEVER hold or use a refresh token.
	if cfg.RefreshToken != "" {
		cfg.RefreshToken = ""
		changed = true
	}
	if !cfg.NoRefresh {
		cfg.NoRefresh = true
		changed = true
	}

	src := "broker"
	if s := strings.TrimSpace(resp.Source); s != "" && !hasControlChars(s) {
		if strings.HasPrefix(s, "broker:") || s == "broker" {
			src = s
		} else {
			src = "broker:" + s
		}
	}
	if cfg.TokenSource != src {
		cfg.TokenSource = src
		changed = true
	}

	ts := strings.TrimSpace(resp.UpdatedAt)
	if ts == "" {
		ts = strings.TrimSpace(resp.CapturedAt)
	}
	if ts != "" && !hasControlChars(ts) && cfg.TokenTime != ts {
		cfg.TokenTime = ts
		changed = true
	}

	exp := strings.TrimSpace(resp.ExpiresAt)
	if exp != "" && !hasControlChars(exp) {
		if cfg.TokenExpiresAt != exp {
			cfg.TokenExpiresAt = exp
			changed = true
		}
	} else if tokenChanged && cfg.TokenExpiresAt != "" {
		cfg.TokenExpiresAt = ""
		changed = true
	}

	if etag := strings.TrimSpace(resp.ETag); etag != "" && !hasControlChars(etag) && cfg.BrokerETag != etag {
		cfg.BrokerETag = etag
		changed = true
	}

	// Defense-in-depth: never apply an untrusted base_url from a broker.
	if validBase, ok := ValidateRayNeoBaseURL(resp.BaseURL); ok && validBase != "" && cfg.BaseURL != validBase {
		cfg.BaseURL = validBase
		changed = true
	}
	if reg := strings.TrimSpace(resp.Region); reg != "" && !hasControlChars(reg) && cfg.Region != reg {
		cfg.Region = reg
		changed = true
	}
	if resp.AccountID > 0 && cfg.AccountID != resp.AccountID {
		if cfg.AccountID != 0 {
			if strings.TrimSpace(resp.Account) == "" && cfg.Account != "" {
				cfg.Account = ""
			}
			if strings.TrimSpace(resp.DeviceID) == "" && cfg.DeviceID != "" {
				cfg.DeviceID = ""
			}
			if len(cfg.ClientHeaders) > 0 {
				cfg.ClientHeaders = nil
			}
		}
		cfg.AccountID = resp.AccountID
		changed = true
	}
	if acct := strings.TrimSpace(resp.Account); acct != "" && !hasControlChars(acct) && cfg.Account != acct {
		cfg.Account = acct
		changed = true
	}
	if dev := strings.TrimSpace(resp.DeviceID); dev != "" && !hasControlChars(dev) && cfg.DeviceID != dev {
		cfg.DeviceID = dev
		changed = true
	}

	filtered := FilterClientHeaders(resp.ClientHeaders)
	if len(filtered) > 0 {
		if cfg.ClientHeaders == nil {
			cfg.ClientHeaders = make(map[string]string, len(filtered))
		}
		for k, v := range filtered {
			if cfg.ClientHeaders[k] != v {
				cfg.ClientHeaders[k] = v
				changed = true
			}
		}
	}

	return changed
}

// FilterClientHeaders returns a new map containing only lowercase allowlisted
// client headers with safe non-empty values.
func FilterClientHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string)
	for k, v := range in {
		lk := strings.ToLower(strings.TrimSpace(k))
		lv := strings.TrimSpace(v)
		if allowedClientHeaders[lk] && lv != "" && len(lv) <= maxHeaderValLen && !hasControlChars(lv) {
			out[lk] = lv
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TokenFingerprint returns the first 12 lowercase hex characters of
// SHA-256(token), matching the broker's token_fp calculation.
func TokenFingerprint(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

// ParseExpiry parses an RFC3339/RFC3339Nano timestamp and reports whether it is
// at or before now along with the remaining duration (clamped to 0).
func ParseExpiry(expiresAt string, now time.Time) (expired bool, remaining time.Duration, ok bool) {
	s := strings.TrimSpace(expiresAt)
	if s == "" {
		return false, 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, err = time.Parse(time.RFC3339, s)
		if err != nil {
			return false, 0, false
		}
	}
	rem := t.Sub(now)
	if rem <= 0 {
		return true, 0, true
	}
	return false, rem, true
}

func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func isPrintableASCIINoSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func extractBrokerError(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Error) != "" {
		msg := strings.TrimSpace(e.Error)
		if len(msg) > 200 {
			msg = msg[:200] + "..."
		}
		return msg
	}
	return ""
}

func sanitizeText(msg string, secrets ...string) string {
	out := msg
	for _, s := range secrets {
		if s != "" {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	return out
}
