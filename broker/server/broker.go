package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	serviceName    = "rayneo-token-broker"
	serviceVersion = "1"

	maxBodyBytes     = 64 * 1024
	maxTokenLen      = 512
	maxSourceLen     = 64
	maxHeaderValLen  = 1024
	defaultTTLSecond = 7200
	maxTTLSecond     = 86400
)

// allowedClientHeaders mirrors internal/har/har.go exactly (15 safe headers).
// Never include xtcl-*, authorization, host, content-*, accept-encoding, or cookie.
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

// TokenState is the persisted single-slot access-token mirror state.
// Refresh tokens are intentionally absent and must never be stored.
type TokenState struct {
	Revision         int64             `json:"revision"`
	AccessToken      string            `json:"access_token"`
	TokenFP          string            `json:"token_fp"`
	Source           string            `json:"source"`
	CapturedAt       string            `json:"captured_at"`
	UpdatedAt        string            `json:"updated_at"`
	ExpiresAt        string            `json:"expires_at"`
	ExpiresEstimated bool              `json:"expires_estimated"`
	BaseURL          string            `json:"base_url"`
	Region           string            `json:"region"`
	Account          string            `json:"account"`
	AccountID        int64             `json:"account_id"`
	DeviceID         string            `json:"device_id"`
	ClientHeaders    map[string]string `json:"client_headers"`
}

func (s *TokenState) clone() *TokenState {
	if s == nil {
		return nil
	}
	cp := *s
	cp.ClientHeaders = make(map[string]string, len(s.ClientHeaders))
	for k, v := range s.ClientHeaders {
		cp.ClientHeaders[k] = v
	}
	return &cp
}

// postTokenRequest accepts only the V1 contract fields. Unknown fields
// (including refresh_token / refreshToken) are silently ignored and stripped.
type postTokenRequest struct {
	AccessToken   string            `json:"access_token"`
	ExpiresIn     int64             `json:"expires_in"`
	CapturedAt    string            `json:"captured_at"`
	Source        string            `json:"source"`
	BaseURL       string            `json:"base_url"`
	Region        string            `json:"region"`
	Account       string            `json:"account"`
	AccountID     int64             `json:"account_id"`
	DeviceID      string            `json:"device_id"`
	ClientHeaders map[string]string `json:"client_headers"`
}

type postTokenResponse struct {
	OK               bool   `json:"ok"`
	Updated          bool   `json:"updated"`
	Revision         int64  `json:"revision"`
	TokenFP          string `json:"token_fp"`
	UpdatedAt        string `json:"updated_at"`
	ExpiresAt        string `json:"expires_at"`
	ExpiresEstimated bool   `json:"expires_estimated"`
}

type getTokenResponse struct {
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
}

// Store holds broker token state with RWMutex synchronization and optional
// atomic 0600 file persistence.
type Store struct {
	mu        sync.RWMutex
	statePath string
	state     *TokenState
}

// NewStore creates a Store backed by statePath (or memory-only if statePath is empty)
// and loads existing persisted state from disk if present.
func NewStore(statePath string) (*Store, error) {
	st := &Store{statePath: statePath}
	if statePath != "" {
		if err := st.loadLocked(); err != nil {
			return nil, err
		}
	}
	return st, nil
}

func (s *Store) loadLocked() error {
	data, err := os.ReadFile(s.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read state file: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var loaded TokenState
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse state file: %w", err)
	}
	if strings.TrimSpace(loaded.AccessToken) == "" {
		return nil
	}
	loaded.AccessToken = strings.TrimSpace(loaded.AccessToken)
	if loaded.TokenFP == "" {
		loaded.TokenFP = tokenFingerprint(loaded.AccessToken)
	}
	loaded.ClientHeaders = filterClientHeaders(loaded.ClientHeaders)
	s.state = &loaded
	return nil
}

func (s *Store) persistLocked(st *TokenState) error {
	if s.statePath == "" {
		return nil
	}
	if st == nil {
		if err := os.Remove(s.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove state file: %w", err)
		}
		return nil
	}
	dir := filepath.Dir(s.statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	payload, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".rayneo-broker-state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("chmod temp state file: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		cleanup()
		return fmt.Errorf("write temp state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp state file: %w", err)
	}
	if err := os.Rename(tmpPath, s.statePath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename state file: %w", err)
	}
	return nil
}

// Get returns a defensive copy of the current token state, or nil if unset.
func (s *Store) Get() *TokenState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.clone()
}

// Clear removes stored token state from memory and disk.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.persistLocked(nil); err != nil {
		return err
	}
	s.state = nil
	return nil
}

// Upsert validates, merges, and persists an incoming token payload atomically.
func (s *Store) Upsert(req postTokenRequest, now time.Time) (postTokenResponse, int, string, error) {
	accessToken := strings.TrimSpace(req.AccessToken)
	if len(accessToken) < 1 || len(accessToken) > maxTokenLen || hasControlChars(accessToken) {
		return postTokenResponse{}, http.StatusBadRequest, "access_token is required (1..512 chars)", nil
	}

	var normalizedCapturedAt string
	if rawCap := strings.TrimSpace(req.CapturedAt); rawCap != "" {
		parsed, err := parseRFC3339(rawCap)
		if err != nil {
			return postTokenResponse{}, http.StatusBadRequest, "invalid captured_at (expected RFC3339)", nil
		}
		normalizedCapturedAt = formatUTCSeconds(parsed)
	}

	source := sanitizeShortString(req.Source, maxSourceLen)
	baseURL := strings.TrimRight(sanitizeShortString(req.BaseURL, 256), "/")
	region := sanitizeShortString(req.Region, 64)
	account := sanitizeShortString(req.Account, 256)
	deviceID := sanitizeShortString(req.DeviceID, 256)
	accountID := req.AccountID
	if accountID < 0 {
		accountID = 0
	}
	incomingHeaders := filterClientHeaders(req.ClientHeaders)

	expiresIn := req.ExpiresIn
	if expiresIn < 0 {
		expiresIn = 0
	}
	if expiresIn > maxTTLSecond {
		expiresIn = maxTTLSecond
	}

	nowUTC := now.UTC().Truncate(time.Second)
	nowStr := nowUTC.Format(time.RFC3339)
	fp := tokenFingerprint(accessToken)

	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.state
	isNewToken := existing == nil || existing.AccessToken != accessToken

	next := &TokenState{
		ClientHeaders: map[string]string{},
	}
	if existing != nil {
		next = existing.clone()
	}

	changed := false

	if isNewToken {
		changed = true
		next.AccessToken = accessToken
		next.TokenFP = fp
		if source != "" {
			next.Source = source
		} else {
			next.Source = "manual"
		}
		if normalizedCapturedAt != "" {
			next.CapturedAt = normalizedCapturedAt
		} else {
			next.CapturedAt = nowStr
		}
		if expiresIn > 0 {
			next.ExpiresAt = nowUTC.Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
			next.ExpiresEstimated = false
		} else {
			next.ExpiresAt = nowUTC.Add(defaultTTLSecond * time.Second).Format(time.RFC3339)
			next.ExpiresEstimated = true
		}
	} else {
		// Same token: preserve existing expires_at & expires_estimated unless explicit expires_in > 0.
		if expiresIn > 0 {
			newExp := nowUTC.Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
			if next.ExpiresAt != newExp || next.ExpiresEstimated {
				next.ExpiresAt = newExp
				next.ExpiresEstimated = false
				changed = true
			}
		}
		if source != "" {
			if next.Source == "" || (isAuthoritativeSource(source) && !isAuthoritativeSource(next.Source)) {
				if next.Source != source {
					next.Source = source
					changed = true
				}
			}
		}
		if next.CapturedAt == "" {
			if normalizedCapturedAt != "" {
				next.CapturedAt = normalizedCapturedAt
			} else {
				next.CapturedAt = nowStr
			}
			changed = true
		}
	}

	// Merge optional metadata fields without wiping existing non-empty values.
	if baseURL != "" && next.BaseURL != baseURL {
		next.BaseURL = baseURL
		changed = true
	}
	if region != "" && next.Region != region {
		next.Region = region
		changed = true
	}
	if account != "" && next.Account != account {
		next.Account = account
		changed = true
	}
	if accountID > 0 && next.AccountID != accountID {
		next.AccountID = accountID
		changed = true
	}
	if deviceID != "" && next.DeviceID != deviceID {
		next.DeviceID = deviceID
		changed = true
	}
	for k, v := range incomingHeaders {
		if next.ClientHeaders[k] != v {
			next.ClientHeaders[k] = v
			changed = true
		}
	}

	if changed {
		if existing != nil {
			next.Revision = existing.Revision + 1
		} else {
			next.Revision = 1
		}
		next.UpdatedAt = nowStr
		if err := s.persistLocked(next); err != nil {
			return postTokenResponse{}, http.StatusInternalServerError, "failed to persist token state", err
		}
		s.state = next
	}

	cur := s.state
	return postTokenResponse{
		OK:               true,
		Updated:          changed,
		Revision:         cur.Revision,
		TokenFP:          cur.TokenFP,
		UpdatedAt:        cur.UpdatedAt,
		ExpiresAt:        cur.ExpiresAt,
		ExpiresEstimated: cur.ExpiresEstimated,
	}, http.StatusOK, "", nil
}

// Broker implements the V1 Token Broker HTTP API.
type Broker struct {
	secret string
	store  *Store
	now    func() time.Time
	logger *log.Logger
}

// NewBroker constructs a Broker http.Handler.
func NewBroker(store *Store, secret string, logger *log.Logger) *Broker {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &Broker{
		secret: strings.TrimSpace(secret),
		store:  store,
		now:    time.Now,
		logger: logger,
	}
}

func (b *Broker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	path := r.URL.Path

	if path == "/health" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
				"ok":    false,
				"error": "method not allowed",
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"service": serviceName,
			"version": serviceVersion,
		})
		return
	}

	// All endpoints other than GET /health require configured BROKER_SECRET and Bearer auth.
	if b.secret == "" {
		b.logger.Printf("rejecting %s %s: status=503 reason=unconfigured_secret", r.Method, path)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "broker secret not configured",
		})
		return
	}
	if !verifyBearerHeader(r.Header.Get("Authorization"), b.secret) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="rayneo-token-broker"`)
		b.logger.Printf("rejecting %s %s: status=401 reason=unauthorized", r.Method, path)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"ok":    false,
			"error": "unauthorized",
		})
		return
	}

	if path != "/v1/token" {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "not found",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		b.handleGetToken(w, r)
	case http.MethodPost:
		b.handlePostToken(w, r)
	case http.MethodDelete:
		b.handleDeleteToken(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"ok":    false,
			"error": "method not allowed",
		})
	}
}

func (b *Broker) handleGetToken(w http.ResponseWriter, r *http.Request) {
	st := b.store.Get()
	if st == nil {
		b.logger.Printf("GET /v1/token status=404")
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "no token synced yet",
		})
		return
	}

	etag := formatETag(st.Revision, st.TokenFP)
	w.Header().Set("ETag", etag)

	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		b.logger.Printf("GET /v1/token status=304 rev=%d fp=%s", st.Revision, st.TokenFP)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	expired, remaining := computeExpiry(st.ExpiresAt, b.now())
	headers := st.ClientHeaders
	if headers == nil {
		headers = map[string]string{}
	}

	b.logger.Printf("GET /v1/token status=200 rev=%d fp=%s expired=%t", st.Revision, st.TokenFP, expired)
	writeJSON(w, http.StatusOK, getTokenResponse{
		OK:               true,
		Revision:         st.Revision,
		AccessToken:      st.AccessToken,
		TokenFP:          st.TokenFP,
		Source:           st.Source,
		CapturedAt:       st.CapturedAt,
		UpdatedAt:        st.UpdatedAt,
		ExpiresAt:        st.ExpiresAt,
		ExpiresEstimated: st.ExpiresEstimated,
		Expired:          expired,
		RemainingSeconds: remaining,
		BaseURL:          st.BaseURL,
		Region:           st.Region,
		Account:          st.Account,
		AccountID:        st.AccountID,
		DeviceID:         st.DeviceID,
		ClientHeaders:    headers,
	})
}

func (b *Broker) handlePostToken(w http.ResponseWriter, r *http.Request) {
	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	defer limited.Close()

	bodyBytes, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			b.logger.Printf("POST /v1/token status=413 reason=body_too_large")
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"ok":    false,
				"error": "request body too large",
			})
			return
		}
		b.logger.Printf("POST /v1/token status=400 reason=read_error")
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "failed to read request body",
		})
		return
	}

	trimmed := strings.TrimSpace(string(bodyBytes))
	if len(trimmed) == 0 || trimmed[0] != '{' {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "invalid json body",
		})
		return
	}

	var req postTokenRequest
	dec := json.NewDecoder(strings.NewReader(trimmed))
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "invalid json body",
		})
		return
	}
	// Reject trailing tokens after the JSON object.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": "invalid json body",
		})
		return
	}

	resp, status, errMsg, internalErr := b.store.Upsert(req, b.now())
	if internalErr != nil {
		b.logger.Printf("POST /v1/token status=%d err=%v", status, internalErr)
		writeJSON(w, status, map[string]any{
			"ok":    false,
			"error": errMsg,
		})
		return
	}
	if status != http.StatusOK {
		b.logger.Printf("POST /v1/token status=%d reason=%q", status, errMsg)
		writeJSON(w, status, map[string]any{
			"ok":    false,
			"error": errMsg,
		})
		return
	}

	b.logger.Printf("POST /v1/token status=200 updated=%t rev=%d fp=%s", resp.Updated, resp.Revision, resp.TokenFP)
	writeJSON(w, http.StatusOK, resp)
}

func (b *Broker) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := b.store.Clear(); err != nil {
		b.logger.Printf("DELETE /v1/token status=500 err=%v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok":    false,
			"error": "failed to clear token state",
		})
		return
	}
	b.logger.Printf("DELETE /v1/token status=200 cleared=true")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"cleared": true,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func verifyBearerHeader(authHeader, expectedSecret string) bool {
	if expectedSecret == "" {
		return false
	}
	h := strings.TrimSpace(authHeader)
	spaceIdx := strings.IndexByte(h, ' ')
	if spaceIdx <= 0 {
		return false
	}
	scheme := h[:spaceIdx]
	if !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	provided := strings.TrimSpace(h[spaceIdx+1:])
	if provided == "" {
		return false
	}
	// Hash both secrets to fixed 32-byte SHA-256 digests so length differences
	// cannot leak via timing.
	hProvided := sha256.Sum256([]byte(provided))
	hExpected := sha256.Sum256([]byte(expectedSecret))
	return subtle.ConstantTimeCompare(hProvided[:], hExpected[:]) == 1
}

func tokenFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func formatETag(revision int64, tokenFP string) string {
	return fmt.Sprintf(`"rev-%d-%s"`, revision, tokenFP)
}

func etagMatches(ifNoneMatch, currentETag string) bool {
	hdr := strings.TrimSpace(ifNoneMatch)
	if hdr == "" {
		return false
	}
	if hdr == "*" {
		return true
	}
	for _, part := range strings.Split(hdr, ",") {
		tag := strings.TrimSpace(part)
		if tag == "" {
			continue
		}
		if tag == "*" {
			return true
		}
		if strings.HasPrefix(tag, "W/") || strings.HasPrefix(tag, "w/") {
			tag = strings.TrimSpace(tag[2:])
		}
		if tag == currentETag {
			return true
		}
	}
	return false
}

func filterClientHeaders(in map[string]string) map[string]string {
	out := make(map[string]string)
	for rawK, rawV := range in {
		k := strings.ToLower(strings.TrimSpace(rawK))
		if !allowedClientHeaders[k] {
			continue
		}
		v := strings.TrimSpace(rawV)
		if v == "" || len(v) > maxHeaderValLen || hasControlChars(v) {
			continue
		}
		out[k] = v
	}
	return out
}

func computeExpiry(expiresAtStr string, now time.Time) (expired bool, remainingSeconds int64) {
	exp, err := parseRFC3339(expiresAtStr)
	if err != nil {
		return true, 0
	}
	diff := exp.Sub(now.UTC())
	if diff <= 0 {
		return true, 0
	}
	return false, int64((diff + time.Second - 1) / time.Second)
}

func parseRFC3339(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func formatUTCSeconds(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func isAuthoritativeSource(src string) bool {
	return src == "loon:refresh" || src == "loon:login"
}

func hasControlChars(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func sanitizeShortString(s string, maxLen int) string {
	v := strings.TrimSpace(s)
	if v == "" || hasControlChars(v) {
		return ""
	}
	if len(v) > maxLen {
		return v[:maxLen]
	}
	return v
}
