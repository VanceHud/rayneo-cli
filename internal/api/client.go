// Package api is a signed HTTP client for the RayNeo XR gateway.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/cache"
	"github.com/VanceHud/rayneo-cli/internal/config"
	"github.com/VanceHud/rayneo-cli/internal/sign"
)

type Client struct {
	Cfg   *config.Config
	HTTP  *http.Client
	Dump  bool              // print signature diagnostics
	Extra map[string]string // extra/override headers (-H)
	Cache *cache.Cache      // optional disk cache
	Hit   bool              // last call served from cache
	Form  string            // non-empty => send this as an urlencoded body
}

func New(cfg *config.Config) *Client {
	return &Client{Cfg: cfg, HTTP: &http.Client{Timeout: 25 * time.Second}}
}

// isSuccess reports {"code":0,...}.
func isSuccess(b []byte) bool {
	var r struct {
		Code *int `json:"code"`
	}
	if json.Unmarshal(b, &r) != nil || r.Code == nil {
		return false
	}
	return *r.Code == 0
}

// appHeaders mirrors what the mobile client sends (only nonce/timestamp are signed).
func (c *Client) appHeaders() map[string]string {
	h := map[string]string{
		"user-agent":       "Dart/3.11 (dart:io)",
		"platform":         "android",
		"env":              "android",
		"locale":           "zh-CN",
		"region":           c.Cfg.RegionCode(),
		"time-zone":        "Asia/Shanghai",
		"app-version":      config.AppVersion,
		"version":          config.AppVersion,
		"app-build-number": "1",
		"phone-os-version": "15",
		"phone-model":      "rayneo-cli",
		"device-id":        c.Cfg.DeviceID,
		"deviceid":         c.Cfg.DeviceID,
		"device_id":        c.Cfg.DeviceID,
		"device-code":      c.Cfg.DeviceID,
	}
	if c.Cfg.AccountID != 0 {
		id := fmt.Sprintf("%d", c.Cfg.AccountID)
		h["account-id"] = id
		h["accountid"] = id
		h["account_id"] = id
		h["xr-account-id"] = id
	}
	for k, v := range c.Cfg.ClientHeaders {
		if v != "" {
			h[k] = v
		}
	}
	return h
}

// Call performs a signed request and returns the parsed response body.
func (c *Client) Call(method, path string, query url.Values, payload any) (*http.Response, []byte, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, nil, err
		}
	}
	q := ""
	if len(query) > 0 {
		q = query.Encode()
	}
	if c.Form != "" {
		body = []byte(c.Form)
		q = ""
	}

	// disk cache: past lifelog data never changes, so avoid the round trip
	if c.Cache != nil {
		if cached, ok := c.Cache.Get(strings.ToUpper(method), path, q, body); ok {
			c.Hit = true
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"X-Rayneo-Cache": []string{"hit"}},
			}, cached, nil
		}
		c.Hit = false
	}
	u := strings.TrimRight(c.Cfg.BaseURL, "/") + path
	if q != "" {
		u += "?" + q
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(strings.ToUpper(method), u, rdr)
	if err != nil {
		return nil, nil, err
	}

	res := sign.Sign(strings.ToUpper(method), path, q, string(body), "", "")
	for k, v := range c.appHeaders() {
		req.Header.Set(k, v)
	}
	if len(body) > 0 {
		if c.Form != "" {
			req.Header.Set("content-type", "application/x-www-form-urlencoded")
		} else {
			req.Header.Set("content-type", "application/json")
		}
	}
	req.Header.Set(sign.HeaderApp, res.AppID)
	req.Header.Set(sign.HeaderNonce, res.Nonce)
	req.Header.Set(sign.HeaderTS, res.Timestamp)
	req.Header.Set(sign.HeaderAuth, res.AuthHeader)
	if c.Cfg.Token != "" {
		req.Header.Set(sign.HeaderToken, c.Cfg.Token)
	}
	for k, v := range c.Extra {
		req.Header.Set(k, v)
	}
	if c.Dump {
		fmt.Printf("--- %s %s\n--- canonical\n%s\n--- sts\n%s\n--- auth\n%s\n",
			method, path, res.Canonical, res.StringToSign, res.AuthHeader)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if c.Cache != nil && resp.StatusCode == 200 && isSuccess(data) {
		c.Cache.Put(strings.ToUpper(method), path, q, body, data,
			cache.AutoTTL(q, string(body)))
	}
	return resp, data, err
}
