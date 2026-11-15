// Package config stores CLI state: credentials and device identity.
package config

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AppVersion mirrors the mobile client so the gateway sees a familiar value.
const AppVersion = "1.0.21"

type Config struct {
	BaseURL       string            `json:"base_url"`
	Token         string            `json:"access_token,omitempty"`
	RefreshToken  string            `json:"refresh_token,omitempty"`
	NoRefresh     bool              `json:"no_refresh,omitempty"`
	TokenSource   string            `json:"token_source,omitempty"`
	TokenTime     string            `json:"token_captured_at,omitempty"`
	Account       string            `json:"account,omitempty"`
	AccountID     int64             `json:"account_id,omitempty"`
	Region        string            `json:"region"` // cn | us | sg | eu
	DeviceID      string            `json:"device_id"`
	ClientHeaders map[string]string `json:"client_headers,omitempty"`
}

// CanRefresh reports whether token renewal is allowed and has a refresh token.
func (c *Config) CanRefresh() bool {
	return !c.NoRefresh && c.RefreshToken != ""
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
