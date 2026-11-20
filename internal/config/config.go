// Package config stores CLI state: credentials and device identity.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppVersion mirrors the mobile client so the gateway sees a familiar value.
const AppVersion = "1.0.21"

type Config struct {
	BaseURL        string            `json:"base_url"`
	Token          string            `json:"access_token,omitempty"`
	RefreshToken   string            `json:"refresh_token,omitempty"`
	NoRefresh      bool              `json:"no_refresh,omitempty"`
	TokenSource    string            `json:"token_source,omitempty"`
	TokenTime      string            `json:"token_captured_at,omitempty"`
	TokenExpiresAt string            `json:"token_expires_at,omitempty"`
	BrokerURL      string            `json:"broker_url,omitempty"`
	BrokerSecret   string            `json:"broker_secret,omitempty"`
	BrokerETag     string            `json:"broker_etag,omitempty"`
	Account        string            `json:"account,omitempty"`
	AccountID      int64             `json:"account_id,omitempty"`
	Region         string            `json:"region"` // cn | us | sg | eu
	DeviceID       string            `json:"device_id"`
	ClientHeaders  map[string]string `json:"client_headers,omitempty"`
}

// EffectiveBroker returns the broker base URL and Bearer secret, preferring
// RAYNEO_BROKER_URL / RAYNEO_BROKER_SECRET environment variables over stored
// config values.
func (c *Config) EffectiveBroker() (url, secret string) {
	url = strings.TrimSpace(os.Getenv("RAYNEO_BROKER_URL"))
	if url == "" && c != nil {
		url = strings.TrimSpace(c.BrokerURL)
	}
	url = strings.TrimRight(url, "/")

	secret = strings.TrimSpace(os.Getenv("RAYNEO_BROKER_SECRET"))
	if secret == "" && c != nil {
		secret = strings.TrimSpace(c.BrokerSecret)
	}
	return url, secret
}

// HasBroker reports whether both a broker URL and secret are configured
// (via environment variables or stored config).
func (c *Config) HasBroker() bool {
	u, s := c.EffectiveBroker()
	return u != "" && s != ""
}

// CanRefresh reports whether token renewal is allowed and has a refresh token.
// Token refresh is always disabled when NoRefresh is set or when a Token Broker
// is configured, so the CLI never invalidates the mobile App's session.
func (c *Config) CanRefresh() bool {
	if c == nil || c.NoRefresh || c.HasBroker() {
		return false
	}
	return c.RefreshToken != ""
}

// RegionCode maps the stored region to the gateway's region header value.
func (c *Config) RegionCode() string {
	if c.Region == "" {
		return "cn"
	}
	return c.Region
}

func dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	d := filepath.Join(base, "rayneo-cli")
	return d, os.MkdirAll(d, 0o700)
}

// CacheDir is where API responses are cached.
func CacheDir() string {
	d, err := dir()
	if err != nil {
		return filepath.Join(os.TempDir(), "rayneo-cli-cache")
	}
	return filepath.Join(d, "cache")
}

func path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.json"), nil
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Load returns the stored config or defaults.
func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	c := &Config{BaseURL: "https://xr-api-p.rayneo.cn", Region: "cn"}
	b, err := os.ReadFile(p)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	} else if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.BaseURL == "" {
		c.BaseURL = "https://xr-api-p.rayneo.cn"
	}
	if c.Region == "" {
		c.Region = "cn"
	}
	if c.DeviceID == "" {
		c.DeviceID = uuid4()
		_ = c.Save()
	}
	return c, nil
}

// Save writes the config atomically with 0600 perms.
func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
