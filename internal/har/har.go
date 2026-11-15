// Package har extracts RayNeo access tokens and client metadata from HTTP
// Archive (.har) captures without touching refresh tokens, so using the CLI
// never invalidates an active login on another device.
package har

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session holds credentials and client context extracted from a HAR capture.
// RefreshToken is intentionally absent: refreshing invalidates the mobile app's
// current access token.
type Session struct {
	Token         string
	BaseURL       string
	Region        string
	Account       string
	AccountID     int64
	DeviceID      string
	ClientHeaders map[string]string
	CapturedAt    time.Time
	EntriesTotal  int
	TokenUses     int
	VerifiedOK    int
}

// allowedClientHeaders lists non-signed device/app headers safe to mirror so
// requests match the captured mobile client's identity. Never include xtcl-*,
// accept-encoding (disables Go's auto-gunzip), host, or content-*.
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

type harFile struct {
	Log struct {
		Entries []harEntry `json:"entries"`
	} `json:"log"`
}

type harEntry struct {
	StartedDateTime string      `json:"startedDateTime"`
	Request         harRequest  `json:"request"`
	Response        harResponse `json:"response"`
}

type harRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers []harHeader `json:"headers"`
}

type harResponse struct {
	Status  int        `json:"status"`
	Content harContent `json:"content"`
}

type harContent struct {
	Encoding string `json:"encoding"`
	Text     string `json:"text"`
}

type harHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type candidate struct {
	token         string
	firstIdx      int
	uses          int
	okCount       int
	err401Count   int
	latestTime    time.Time
	latestOKTime  time.Time
	latest401Time time.Time
	baseURL       string
	region        string
	account       string
	accountID     int64
	deviceID      string
	clientHeaders map[string]string
}

func (c *candidate) isExpired() bool {
	if c.okCount == 0 && c.err401Count > 0 {
		return true
	}
	if !c.latest401Time.IsZero() && (c.latestOKTime.IsZero() || c.latest401Time.After(c.latestOKTime)) {
		return true
	}
	return false
}

// Extract reads and parses a .har file from disk.
func Extract(path string) (*Session, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse extracts the newest valid RayNeo access token from raw HAR JSON bytes.
func Parse(data []byte) (*Session, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var hf harFile
	if err := json.Unmarshal(data, &hf); err != nil {
		return nil, fmt.Errorf("invalid HAR JSON: %w", err)
	}
	if len(hf.Log.Entries) == 0 {
		return nil, fmt.Errorf("HAR contains no entries")
	}

	cands := map[string]*candidate{}
	getCand := func(tok string, idx int) *candidate {
		c, ok := cands[tok]
		if !ok {
			c = &candidate{
				token:         tok,
				firstIdx:      idx,
				clientHeaders: map[string]string{},
			}
			cands[tok] = c
		}
		return c
	}

	var fallbackBaseURL, fallbackRegion, fallbackAccount, fallbackDeviceID string
	var fallbackAccountID int64
	fallbackHeaders := map[string]string{}

	for idx, e := range hf.Log.Entries {
		if e.Request.URL == "" {
			continue
		}
		u, err := url.Parse(e.Request.URL)
		if err != nil {
			continue
		}
		host := strings.ToLower(u.Hostname())

		hmap := make(map[string]string, len(e.Request.Headers))
		for _, h := range e.Request.Headers {
			k := strings.ToLower(strings.TrimSpace(h.Name))
			if k != "" {
				hmap[k] = strings.TrimSpace(h.Value)
			}
		}

		hasXTCL := hmap["xtcl-access-token"] != "" || hmap["xtcl-app"] != "" || hmap["xtcl-authorization"] != ""
		isRayNeo := strings.Contains(host, "rayneo.")
		if !hasXTCL && !isRayNeo {
			continue
		}

		ts := parseEntryTime(e.StartedDateTime, hmap["xtcl-timestamp"])

		var entryBaseURL string
		if strings.HasPrefix(host, "xr-api") && u.Scheme != "" && u.Host != "" {
			entryBaseURL = u.Scheme + "://" + u.Host
			if fallbackBaseURL == "" {
				fallbackBaseURL = entryBaseURL
			}
		}
		entryRegion := hmap["region"]
		if entryRegion != "" && fallbackRegion == "" {
			fallbackRegion = entryRegion
		}
		entryAccountID := extractHeaderAccountID(hmap, u)
		if entryAccountID != 0 && fallbackAccountID == 0 {
			fallbackAccountID = entryAccountID
		}
		entryDeviceID := hmap["device-code"]
		if entryDeviceID == "" {
			entryDeviceID = hmap["device-id"]
		}
		if entryDeviceID != "" && fallbackDeviceID == "" {
			fallbackDeviceID = entryDeviceID
		}
		for k, v := range hmap {
			if allowedClientHeaders[k] && v != "" {
				if fallbackHeaders[k] == "" {
					fallbackHeaders[k] = v
				}
			}
		}

		// Decode JSON response body when reasonably sized.
		var respCode *int
		var loginToken, respAccount string
		var respAccountID int64
		if len(e.Response.Content.Text) > 0 && len(e.Response.Content.Text) < 2*1024*1024 {
			if raw := decodeContent(e.Response.Content); len(raw) > 0 {
				trimmed := bytes.TrimSpace(raw)
				if len(trimmed) > 0 && trimmed[0] == '{' {
					respCode, loginToken, respAccountID, respAccount = inspectResponseJSON(u.Path, trimmed)
				}
			}
		}
		if entryAccountID == 0 && respAccountID != 0 {
			entryAccountID = respAccountID
		}
		if respAccountID != 0 && fallbackAccountID == 0 {
			fallbackAccountID = respAccountID
		}
		if respAccount != "" && fallbackAccount == "" {
			fallbackAccount = respAccount
		}

		applyMeta := func(c *candidate) {
			if !ts.IsZero() && ts.After(c.latestTime) {
				c.latestTime = ts
				// Prefer headers from the freshest request for this token.
				for k, v := range hmap {
					if allowedClientHeaders[k] && v != "" {
						c.clientHeaders[k] = v
					}
				}
				if entryDeviceID != "" {
					c.deviceID = entryDeviceID
				}
			} else {
				for k, v := range hmap {
					if allowedClientHeaders[k] && v != "" && c.clientHeaders[k] == "" {
						c.clientHeaders[k] = v
					}
				}
				if c.deviceID == "" && entryDeviceID != "" {
					c.deviceID = entryDeviceID
				}
			}
			if entryBaseURL != "" {
				c.baseURL = entryBaseURL
			}
			if entryRegion != "" {
				c.region = entryRegion
			}
			if entryAccountID != 0 {
				c.accountID = entryAccountID
			}
			if respAccount != "" {
				c.account = respAccount
			}
		}

		// 1) Request header xtcl-access-token — EXCEPT /authapi/token/refresh
		// where xtcl-access-token carries the refreshToken instead of accessToken!
		reqTok := hmap["xtcl-access-token"]
		if reqTok != "" && !isRefreshPath(u.Path) {
			c := getCand(reqTok, idx)
			c.uses++
			is401 := e.Response.Status == 401 || (respCode != nil && *respCode == 401)
			isOK := !is401 && e.Response.Status == 200
			if is401 {
				c.err401Count++
				if !ts.IsZero() && ts.After(c.latest401Time) {
					c.latest401Time = ts
				}
			} else if isOK {
				c.okCount++
				if !ts.IsZero() && ts.After(c.latestOKTime) {
					c.latestOKTime = ts
				}
			}
			applyMeta(c)
		}

		// 2) Auth response body token (e.g. /identify/quickLogin or /authapi/token/refresh).
		// Note: refreshToken in response is deliberately ignored.
		if loginToken != "" && e.Response.Status == 200 && (respCode == nil || *respCode == 0) {
			c := getCand(loginToken, idx)
			c.uses++
			c.okCount++
			if !ts.IsZero() && ts.After(c.latestOKTime) {
				c.latestOKTime = ts
			}
			applyMeta(c)
		}
	}

	if len(cands) == 0 {
		return nil, fmt.Errorf("no xtcl-access-token found in HAR (capture xr-api-*.rayneo.cn HTTPS traffic while opening the RayNeo App)")
	}

	list := make([]*candidate, 0, len(cands))
	for _, c := range cands {
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.isExpired() != b.isExpired() {
			return !a.isExpired()
		}
		aOK, bOK := a.okCount > 0, b.okCount > 0
		if aOK != bOK {
			return aOK
		}
		if !a.latestTime.Equal(b.latestTime) {
			return a.latestTime.After(b.latestTime)
		}
		if a.okCount != b.okCount {
			return a.okCount > b.okCount
		}
		return a.firstIdx < b.firstIdx
	})

	win := list[0]
	if win.isExpired() {
		return nil, fmt.Errorf("access token in HAR already returned 401 (expired during capture); please capture a fresh HAR from the logged-in app")
	}

	if win.baseURL == "" {
		win.baseURL = fallbackBaseURL
	}
	if win.region == "" {
		win.region = fallbackRegion
	}
	if win.accountID == 0 {
		win.accountID = fallbackAccountID
	}
	if win.account == "" {
		win.account = fallbackAccount
	}
	if win.deviceID == "" {
		win.deviceID = fallbackDeviceID
	}
	if len(win.clientHeaders) == 0 && len(fallbackHeaders) > 0 {
		win.clientHeaders = fallbackHeaders
	}

	return &Session{
		Token:         win.token,
		BaseURL:       win.baseURL,
		Region:        win.region,
		Account:       win.account,
		AccountID:     win.accountID,
		DeviceID:      win.deviceID,
		ClientHeaders: win.clientHeaders,
		CapturedAt:    win.latestTime,
		EntriesTotal:  len(hf.Log.Entries),
		TokenUses:     win.uses,
		VerifiedOK:    win.okCount,
	}, nil
}

func isRefreshPath(p string) bool {
	lp := strings.ToLower(p)
	return strings.Contains(lp, "/token/refresh")
}

func isAuthIssuePath(p string) bool {
	lp := strings.ToLower(p)
	return strings.Contains(lp, "/quicklogin") ||
		strings.HasSuffix(lp, "/login") ||
		strings.Contains(lp, "/token/refresh")
}

func parseEntryTime(started, xtclTS string) time.Time {
	if started != "" {
		formats := []string{
			time.RFC3339Nano,
			time.RFC3339,
			"2006-01-02T15:04:05.999999999-0700",
			"2006-01-02T15:04:05-0700",
		}
		for _, layout := range formats {
			if t, err := time.Parse(layout, started); err == nil {
				return t
			}
		}
	}
	if xtclTS != "" {
		if n, err := strconv.ParseInt(xtclTS, 10, 64); err == nil && n > 1_000_000_000 {
			if n > 1_000_000_000_000 {
				return time.UnixMilli(n)
			}
			return time.Unix(n, 0)
		}
	}
	return time.Time{}
}

func extractHeaderAccountID(hmap map[string]string, u *url.URL) int64 {
	for _, k := range []string{"account-id", "accountid", "account_id", "xr-account-id"} {
		if v := hmap[k]; v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	if u != nil && u.RawQuery != "" {
		if v := u.Query().Get("accountId"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				return n
			}
		}
	}
	return 0
}

func decodeContent(c harContent) []byte {
	if c.Text == "" {
		return nil
	}
	var raw []byte
	if strings.EqualFold(c.Encoding, "base64") {
		b, err := base64.StdEncoding.DecodeString(c.Text)
		if err != nil {
			b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(c.Text, "="))
			if err != nil {
				return nil
			}
		}
		raw = b
	} else {
		raw = []byte(c.Text)
	}
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			defer gr.Close()
			if dec, err := io.ReadAll(io.LimitReader(gr, 4*1024*1024)); err == nil {
				return dec
			}
		}
	}
	return raw
}

func inspectResponseJSON(path string, raw []byte) (code *int, authToken string, accountID int64, account string) {
	var env struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil, "", 0, ""
	}
	code = env.Code
	if len(env.Data) == 0 || env.Data[0] != '{' {
		return code, "", 0, ""
	}
	var data struct {
		Token     string `json:"token"`
		AccountID int64  `json:"accountId"`
		Phone     string `json:"phone"`
		Mobile    string `json:"mobile"`
		Email     string `json:"email"`
		UserInfo  *struct {
			AccountID int64  `json:"accountId"`
			Phone     string `json:"phone"`
			Mobile    string `json:"mobile"`
			Email     string `json:"email"`
		} `json:"userInfo"`
	}
	if json.Unmarshal(env.Data, &data) != nil {
		return code, "", 0, ""
	}
	if isAuthIssuePath(path) && data.Token != "" {
		authToken = data.Token
	}
	accountID = data.AccountID
	account = firstNonEmptyStr(data.Phone, data.Mobile, data.Email)
	if data.UserInfo != nil {
		if accountID == 0 {
			accountID = data.UserInfo.AccountID
		}
		if account == "" {
			account = firstNonEmptyStr(data.UserInfo.Phone, data.UserInfo.Mobile, data.UserInfo.Email)
		}
	}
	account = strings.TrimPrefix(account, "+86-")
	return code, authToken, accountID, account
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
