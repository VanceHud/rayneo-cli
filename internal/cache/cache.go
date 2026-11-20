// Package cache stores API responses on disk so repeated queries do not hit
// the network. Lifelog data for past days is effectively immutable, today's
// data changes, and auth endpoints must never be cached.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is the on-disk wrapper.
type Entry struct {
	FetchedAt int64           `json:"fetched_at"`
	TTL       int64           `json:"ttl_seconds"`
	Path      string          `json:"path"`
	Query     string          `json:"query,omitempty"`
	Body      json.RawMessage `json:"body"`
}

type Cache struct {
	Dir     string
	Enabled bool
	Force   bool          // ignore existing entries (still writes)
	TTL     time.Duration // override auto TTL
}

// TTL policies.
const (
	TTLPast        = 30 * 24 * time.Hour // finished days / reverse geocode
	TTLEventDetail = 6 * time.Hour       // event detail / full transcripts / recording clips
	TTLToday       = 5 * time.Minute     // still accumulating (today or future)
	TTLVolatile    = 10 * time.Minute    // counts / tasks / chats
)

// New returns a cache rooted at dir (created lazily).
func New(dir string) *Cache { return &Cache{Dir: dir, Enabled: true} }

// uncacheable lists path prefixes that must always hit the network.
var uncacheable = []string{
	"/identify/",
	"/authapi/",
	"/api/push/",
	"/cloudapi/",
}

// readOnlyPostPaths lists POST endpoints that are strictly read-only queries
// and safe to cache. Any other POST (and all PUT/PATCH/DELETE) is never cached.
var readOnlyPostPaths = map[string]bool{
	"/lifelog/api/v1/calendar":           true,
	"/lifelog/api/v1/journal/detail":     true,
	"/summary-service/suggest/list":      true,
	"/summary-service/summary/list":      true,
	"/clsapi/aiTask/v2/selectPage":       true,
	"/clsapi/aiTask/getDataAllByTaskId":  true,
	"/nlp/rayClaw/listAppSkills":         true,
	"/agent/user-config/list":            true,
}

func uncacheablePath(p string) bool {
	for _, pre := range uncacheable {
		if strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// IsCacheable reports whether a request method + path may be read from or stored
// in the disk cache. Write methods and unknown POST paths are never cached.
func IsCacheable(method, path string) bool {
	if uncacheablePath(path) {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET":
		return true
	case "POST":
		return readOnlyPostPaths[path]
	default:
		return false
	}
}

// AutoTTL decides how long a response stays fresh based on query and body.
func AutoTTL(query, body string) time.Duration {
	return AutoTTLFor("", query, body)
}

// AutoTTLFor decides how long a response stays fresh based on path, query, and body.
//
// Rules:
//   - /profileapi/geo/reverseGeocode => 30 days
//   - today or future YYYY-MM-DD (or current YYYYMM month) => 5 min
//   - past YYYY-MM-DD (or past YYYYMM month) => 30 days
//   - /lifelog/api/v1/events/{id}[/full] or /clsapi/aiTask/getDataAllByTaskId => 6 hours
//   - everything else (counts, tasks, chats, suggestions) => 10 min
func AutoTTLFor(path, query, body string) time.Duration {
	if path == "/profileapi/geo/reverseGeocode" {
		return TTLPast
	}
	blob := query + " " + body
	now := time.Now()
	today := now.Format("2006-01-02")
	compactToday := now.Format("200601")
	thisMonth := now.Format("2006-01")

	if strings.Contains(blob, today) || strings.Contains(blob, compactToday) {
		return TTLToday
	}
	if d := findDate(blob); d != "" {
		if d < today {
			return TTLPast
		}
		return TTLToday
	}
	if m := findMonth(blob); m != "" && m < thisMonth {
		return TTLPast
	}
	if strings.HasPrefix(path, "/lifelog/api/v1/events/") || path == "/clsapi/aiTask/getDataAllByTaskId" {
		return TTLEventDetail
	}
	return TTLVolatile
}

// findMonth returns the first YYYYMM (6 digits) substring.
func findMonth(s string) string {
	for i := 0; i+6 <= len(s); i++ {
		c := s[i : i+6]
		ok := true
		for _, r := range c {
			if r < '0' || r > '9' {
				ok = false
				break
			}
		}
		if ok && c[:2] == "20" {
			return c[:4] + "-" + c[4:]
		}
	}
	return ""
}

// findDate returns the first YYYY-MM-DD substring, if any.
func findDate(s string) string {
	for i := 0; i+10 <= len(s); i++ {
		c := s[i : i+10]
		if c[4] == '-' && c[7] == '-' {
			ok := true
			for j, r := range c {
				if j == 4 || j == 7 {
					continue
				}
				if r < '0' || r > '9' {
					ok = false
					break
				}
			}
			if ok {
				return c
			}
		}
	}
	return ""
}

func (c *Cache) key(method, path, query string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write([]byte(query))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Cache) file(key string) string {
	return filepath.Join(c.Dir, key[:2], key+".json")
}

// Get returns a fresh cached body.
func (c *Cache) Get(method, path, query string, body []byte) ([]byte, bool) {
	if c == nil || !c.Enabled || c.Force || !IsCacheable(method, path) {
		return nil, false
	}
	b, err := os.ReadFile(c.file(c.key(method, path, query, body)))
	if err != nil {
		return nil, false
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		return nil, false
	}
	ttl := time.Duration(e.TTL) * time.Second
	if c.TTL > 0 {
		ttl = c.TTL
	}
	if time.Since(time.Unix(e.FetchedAt, 0)) > ttl {
		return nil, false
	}
	return []byte(e.Body), true
}

// Put stores a successful response.
func (c *Cache) Put(method, path, query string, body, resp []byte, ttl time.Duration) {
	if c == nil || !c.Enabled || !IsCacheable(method, path) || len(resp) == 0 {
		return
	}
	if c.TTL > 0 {
		ttl = c.TTL
	}
	e := Entry{FetchedAt: time.Now().Unix(), TTL: int64(ttl / time.Second), Path: path, Query: query, Body: resp}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	p := c.file(c.key(method, path, query, body))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// Stat summarises the cache.
func (c *Cache) Stat() (files int, bytes int64, fresh, stale int) {
	now := time.Now()
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		files++
		bytes += info.Size()
		if b, err := os.ReadFile(p); err == nil {
			var e Entry
			if json.Unmarshal(b, &e) == nil {
				ttl := time.Duration(e.TTL) * time.Second
				if now.Sub(time.Unix(e.FetchedAt, 0)) <= ttl {
					fresh++
				} else {
					stale++
				}
			}
		}
		return nil
	})
	return
}

// List returns entries sorted by fetch time (newest first).
func (c *Cache) List(limit int) []Entry {
	var out []Entry
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		if b, err := os.ReadFile(p); err == nil {
			var e Entry
			if json.Unmarshal(b, &e) == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].FetchedAt > out[j].FetchedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Clear removes all cached files.
func (c *Cache) Clear() error {
	if c.Dir == "" {
		return fmt.Errorf("empty cache dir")
	}
	return os.RemoveAll(c.Dir)
}

// Prune removes only stale or unreadable cache files, keeping fresh entries intact.
func (c *Cache) Prune() (removed int, bytesFreed int64) {
	if c == nil || c.Dir == "" {
		return 0, 0
	}
	now := time.Now()
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		stale := false
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			stale = true
		} else {
			var e Entry
			if json.Unmarshal(b, &e) != nil {
				stale = true
			} else if now.Sub(time.Unix(e.FetchedAt, 0)) > time.Duration(e.TTL)*time.Second {
				stale = true
			}
		}
		if stale {
			sz := info.Size()
			if os.Remove(p) == nil {
				removed++
				bytesFreed += sz
			}
		}
		return nil
	})
	return removed, bytesFreed
}

// InvalidatePrefix removes cached entries whose stored Path starts with pathPrefix.
func (c *Cache) InvalidatePrefix(pathPrefix string) int {
	if c == nil || c.Dir == "" || pathPrefix == "" {
		return 0
	}
	removed := 0
	_ = filepath.Walk(c.Dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".json") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		var e Entry
		if json.Unmarshal(b, &e) == nil && strings.HasPrefix(e.Path, pathPrefix) {
			if os.Remove(p) == nil {
				removed++
			}
		}
		return nil
	})
	return removed
}
