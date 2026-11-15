package har

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func b64Gzip(s string) string {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(s))
	_ = gw.Close()
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestParseLoonStyleReverseOrderAndGzip(t *testing.T) {
	doc := map[string]any{
		"log": map[string]any{
			"entries": []any{
				// Aborted in-flight request when capture stopped (status 0)
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:05.200+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/profileapi/email/query",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "tok_active_synthetic_0001"},
							map[string]any{"name": "accept-encoding", "value": "gzip"},
							map[string]any{"name": "platform", "value": "ios"},
							map[string]any{"name": "phone-model", "value": "iPhone16,1"},
							map[string]any{"name": "device-code", "value": "00000000-0000-4000-8000-000000000001"},
							map[string]any{"name": "accountid", "value": "100000000000001"},
							map[string]any{"name": "region", "value": "cn"},
						},
					},
					"response": map[string]any{
						"status":  0,
						"content": map[string]any{},
					},
				},
				// Successful request slightly earlier (proxy exports newest-first)
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:02.100+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/lifelog/api/v1/count",
						"headers": []any{
							map[string]any{"name": "Xtcl-Access-Token", "value": "tok_active_synthetic_0001"},
							map[string]any{"name": "region", "value": "cn"},
						},
					},
					"response": map[string]any{
						"status": 200,
						"content": map[string]any{
							"encoding": "base64",
							"text":     b64Gzip(`{"code":0,"msg":"ok","data":{}}`),
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(doc)
	s, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if s.Token != "tok_active_synthetic_0001" {
		t.Fatalf("got token %q", s.Token)
	}
	if s.BaseURL != "https://xr-api-p.rayneo.cn" {
		t.Fatalf("got baseURL %q", s.BaseURL)
	}
	if s.AccountID != 100000000000001 {
		t.Fatalf("got accountID %d", s.AccountID)
	}
	if s.DeviceID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("got deviceID %q", s.DeviceID)
	}
	if s.ClientHeaders["platform"] != "ios" || s.ClientHeaders["phone-model"] != "iPhone16,1" {
		t.Fatalf("unexpected client headers: %#v", s.ClientHeaders)
	}
	if _, bad := s.ClientHeaders["accept-encoding"]; bad {
		t.Fatalf("accept-encoding must not be copied into ClientHeaders")
	}
	if s.VerifiedOK != 1 || s.TokenUses != 2 {
		t.Fatalf("unexpected counts: uses=%d ok=%d", s.TokenUses, s.VerifiedOK)
	}
}

func TestParseSkipsRefreshHeaderAndPicksNewestValidToken(t *testing.T) {
	doc := map[string]any{
		"log": map[string]any{
			"entries": []any{
				// 1) Old token gets 401
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:01+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/lifelog/api/v1/count",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "OLD_EXPIRED_ACCESS_TOKEN"},
						},
					},
					"response": map[string]any{
						"status":  200,
						"content": map[string]any{"text": `{"code":401,"msg":"Authentication failed"}`},
					},
				},
				// 2) App calls /authapi/token/refresh with REFRESH_TOKEN in xtcl-access-token header!
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:02+08:00",
					"request": map[string]any{
						"method": "POST",
						"url":    "https://xr-api-p.rayneo.cn/authapi/token/refresh",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "DANGEROUS_REFRESH_TOKEN_DO_NOT_USE"},
						},
					},
					"response": map[string]any{
						"status":  200,
						"content": map[string]any{"text": `{"code":0,"msg":"ok","data":{"token":"NEW_ACCESS_TOKEN_FROM_REFRESH","expiresIn":7200}}`},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(doc)
	s, err := Parse(b)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if s.Token != "NEW_ACCESS_TOKEN_FROM_REFRESH" {
		t.Fatalf("expected NEW_ACCESS_TOKEN_FROM_REFRESH, got %q", s.Token)
	}
}

func TestParseRejectsAllExpiredTokens(t *testing.T) {
	doc := map[string]any{
		"log": map[string]any{
			"entries": []any{
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:01+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/lifelog/api/v1/count",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "DEAD_TOKEN"},
						},
					},
					"response": map[string]any{
						"status":  200,
						"content": map[string]any{"text": `{"code":401,"msg":"Authentication failed"}`},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(doc)
	if _, err := Parse(b); err == nil {
		t.Fatalf("expected error when only token in HAR already returned 401")
	}
}

func TestExtractFromSyntheticFile(t *testing.T) {
	doc := map[string]any{
		"log": map[string]any{
			"entries": []any{
				map[string]any{
					"startedDateTime": "2026-01-15T10:00:01+08:00",
					"request": map[string]any{
						"method": "GET",
						"url":    "https://xr-api-p.rayneo.cn/lifelog/api/v1/count",
						"headers": []any{
							map[string]any{"name": "xtcl-access-token", "value": "synthetic_file_access_token_0000000000000001"},
							map[string]any{"name": "accountid", "value": "100000000000001"},
							map[string]any{"name": "region", "value": "cn"},
						},
					},
					"response": map[string]any{
						"status":  200,
						"content": map[string]any{"text": `{"code":0,"msg":"ok","data":{}}`},
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "capture.har")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Extract(p)
	if err != nil {
		t.Fatalf("Extract failed: %v", err)
	}
	if s.Token != "synthetic_file_access_token_0000000000000001" || s.BaseURL != "https://xr-api-p.rayneo.cn" || s.AccountID != 100000000000001 || s.VerifiedOK != 1 {
		t.Fatalf("unexpected extracted session: %+v", s)
	}
}
