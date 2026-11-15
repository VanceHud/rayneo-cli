// Package sign implements the TCL1-HMAC-SHA256 request signing used by the
// RayNeo XR gateway (xr-api-*.rayneo.cn).
//
// Signing layout:
//
//	canonicalHeaders = "xtcl-nonce:<nonce>;xtcl-timestamp:<ts>;"
//	canonicalRequest = METHOD \n PATH \n QUERY \n canonicalHeaders \n base64(sha256(body))
//	stringToSign     = "TCL1-HMAC-SHA256" \n <ts> \n base64(sha256(canonicalRequest))
//	signature        = base64( HMAC-SHA256( appKey, stringToSign ) )
//	Authorization    = "TCL1-HMAC-SHA256 Credential=<appId>,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=<sig>"
package sign

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	Algo = "TCL1-HMAC-SHA256"

	HeaderAuth  = "xtcl-authorization"
	HeaderApp   = "xtcl-app"
	HeaderNonce = "xtcl-nonce"
	HeaderTS    = "xtcl-timestamp"
	HeaderToken = "xtcl-access-token"

	SignedHeaders = "xtcl-nonce;xtcl-timestamp;"
)

// DefaultAppID and DefaultAppKey are the built-in v1.0.20 public mobile client
// gateway signing credentials. They can be overridden at runtime via
// RAYNEO_APP_ID and RAYNEO_APP_KEY environment variables.
const (
	DefaultAppID  = "bf16c55936bb4d439c2d7a914a806559"
	DefaultAppKey = "UGuW3frdyM@Ah*oRLSa^3f8C&H47dQ5~"

	AppID  = DefaultAppID
	AppKey = DefaultAppKey
)

// Credentials returns the effective gateway AppID and AppKey, preferring
// RAYNEO_APP_ID and RAYNEO_APP_KEY environment variables when non-empty and
// falling back to DefaultAppID / DefaultAppKey.
func Credentials() (appID, appKey string) {
	appID = strings.TrimSpace(os.Getenv("RAYNEO_APP_ID"))
	if appID == "" {
		appID = DefaultAppID
	}
	appKey = os.Getenv("RAYNEO_APP_KEY")
	if appKey == "" {
		appKey = DefaultAppKey
	}
	return appID, appKey
}

func shaB64(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Nonce returns a 32 hex-char nonce (uuid4 without dashes).
func Nonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

// Timestamp is epoch seconds, as a string.
func Timestamp() string { return fmt.Sprintf("%d", time.Now().Unix()) }

// Result carries everything needed to reproduce a signature.
type Result struct {
	AppID        string
	Nonce        string
	Timestamp    string
	Canonical    string
	StringToSign string
	Signature    string
	AuthHeader   string
}

// Sign computes the authorization header for one request.
// path must start with "/", query is the raw (already encoded) query string.
func Sign(method, path, query, body, nonce, ts string) Result {
	if nonce == "" {
		nonce = Nonce()
	}
	if ts == "" {
		ts = Timestamp()
	}
	appID, appKey := Credentials()

	canonicalHeaders := fmt.Sprintf("xtcl-nonce:%s;xtcl-timestamp:%s;", nonce, ts)
	canonical := method + "\n" + path + "\n" + query + "\n" + canonicalHeaders + "\n" + shaB64(body)
	sts := Algo + "\n" + ts + "\n" + shaB64(canonical)

	mac := hmac.New(sha256.New, []byte(appKey))
	mac.Write([]byte(sts))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	return Result{
		AppID:        appID,
		Nonce:        nonce,
		Timestamp:    ts,
		Canonical:    canonical,
		StringToSign: sts,
		Signature:    sig,
		AuthHeader: fmt.Sprintf("%s Credential=%s,SignedHeaders=%s,Signature=%s",
			Algo, appID, SignedHeaders, sig),
	}
}
