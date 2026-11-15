package sign

import "testing"

// Deterministic synthetic vector for POST with JSON body and empty query line.
func TestSignPostJSONBody(t *testing.T) {
	t.Setenv("RAYNEO_APP_ID", "")
	t.Setenv("RAYNEO_APP_KEY", "")

	body := `{"sendType":3,"bindType":3,"type":3,"loginName":"13800000000","bindName":"13800000000","areaCode":"+86","runtimeType":"phone","validate":null,"sceneType":2}`
	r := Sign("POST", "/identify/common/sendVerifyCode", "", body,
		"0123456789abcdef0123456789abcdef", "1700000000")
	const want = "nJKUQsMBKd/4jItceX67psMADIJ5EWcmEcgyeGAY/e8="
	if r.Signature != want {
		t.Fatalf("signature mismatch\n got %s\nwant %s\ncanonical:\n%s", r.Signature, want, r.Canonical)
	}
	if r.AppID != DefaultAppID {
		t.Fatalf("expected default AppID %q, got %q", DefaultAppID, r.AppID)
	}
	wantAuth := "TCL1-HMAC-SHA256 Credential=bf16c55936bb4d439c2d7a914a806559,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=" + want
	if r.AuthHeader != wantAuth {
		t.Fatalf("auth header mismatch:\n%s", r.AuthHeader)
	}
}

// Deterministic synthetic vector for GET with URL-encoded query string.
func TestSignGetWithQuery(t *testing.T) {
	t.Setenv("RAYNEO_APP_ID", "")
	t.Setenv("RAYNEO_APP_KEY", "")

	r := Sign("GET", "/lifelog/api/v1/events", "date=2026-01-15&timezone=Asia%2FShanghai", "",
		"fedcba9876543210fedcba9876543210", "1700000060")
	const want = "OoJRmghsWc6vTmZ0NMiILdl6ihAP72Qb0022qpzv2tE="
	if r.Signature != want {
		t.Fatalf("signature mismatch\n got %s\nwant %s\ncanonical:\n%s", r.Signature, want, r.Canonical)
	}
}

// Verify runtime credential overrides via RAYNEO_APP_ID and RAYNEO_APP_KEY.
func TestSignEnvOverride(t *testing.T) {
	t.Setenv("RAYNEO_APP_ID", "test-custom-app-id-0001")
	t.Setenv("RAYNEO_APP_KEY", "test-custom-app-key-secret-0001")

	r := Sign("GET", "/lifelog/api/v1/count", "", "",
		"abcdef0123456789abcdef0123456789", "1700000120")
	const want = "PzzlefY7cM9Bkboosb7CVDRuIDt7avzeAsZKpegs41A="
	if r.AppID != "test-custom-app-id-0001" {
		t.Fatalf("expected custom AppID, got %q", r.AppID)
	}
	if r.Signature != want {
		t.Fatalf("signature mismatch with env override\n got %s\nwant %s", r.Signature, want)
	}
	wantAuth := "TCL1-HMAC-SHA256 Credential=test-custom-app-id-0001,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=" + want
	if r.AuthHeader != wantAuth {
		t.Fatalf("auth header mismatch with env override:\n%s", r.AuthHeader)
	}
}
