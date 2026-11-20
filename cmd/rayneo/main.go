// Command rayneo queries RayNeo AI "全天智记" (lifelog) data.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
	"github.com/VanceHud/rayneo-cli/internal/broker"
	"github.com/VanceHud/rayneo-cli/internal/cache"
	"github.com/VanceHud/rayneo-cli/internal/config"
	"github.com/VanceHud/rayneo-cli/internal/har"
)

// version is set at build time via -ldflags "-X main.version=..."
var version = "dev"

func effectiveVersion() string {
	if version != "" {
		return version
	}
	return "dev"
}

func printHelp() {
	helpSpec.Version = effectiveVersion()
	fmt.Printf("%s %s — %s\n\n", helpSpec.Name, helpSpec.Version, helpSpec.Description)
	fmt.Printf("auth: %s\n\n", helpSpec.Auth)
	groups := []struct {
		title string
		names []string
	}{
		{"auth", []string{"sync", "har", "sendcode", "login", "refresh", "whoami", "logout", "config"}},
		{"query (read-only)", []string{"count", "calendar", "events", "event", "journal", "jd", "suggests", "tasks", "chat"}},
		{"transcripts (read-only)", []string{"full", "recordings", "transcript"}},
		{"bulk", []string{"export", "report", "cache"}},
		{"utility", []string{"raw", "help"}},
	}
	byName := map[string]cmdSpec{}
	for _, c := range helpSpec.Commands {
		byName[c.Name] = c
	}
	for _, g := range groups {
		fmt.Printf("%s:\n", g.title)
		for _, n := range g.names {
			c, ok := byName[n]
			if !ok {
				continue
			}
			fmt.Printf("  %-10s %s\n", c.Name, c.Summary)
			for _, a := range c.Args {
				def := ""
				if a.Default != "" {
					def = " (default " + a.Default + ")"
				}
				fmt.Printf("               %-10s %s%s\n", a.Name, a.Desc, def)
			}
			if c.Path != "" {
				fmt.Printf("               -> %s %s\n", c.Method, c.Path)
			}
			if c.Example != "" {
				fmt.Printf("               e.g. %s\n", c.Example)
			}
		}
		fmt.Println()
	}
	fmt.Println("global flags:")
	fmt.Println("  -version             print version and exit")
	fmt.Println("  -broker-url <url>    override token broker URL for this run (or RAYNEO_BROKER_URL)")
	fmt.Println("  -broker-secret <sec> override token broker Bearer secret for this run (or RAYNEO_BROKER_SECRET)")
	fmt.Println("  -har <file.har>      use access token from a .har capture for this run (never refreshes)")
	fmt.Println("  -no-refresh          never renew access token via /authapi/token/refresh (protects other devices)")
	fmt.Println("  -debug               print canonical/stringToSign/authorization and cache hits")
	fmt.Println("  -refresh             force network (ignore local disk cache)")
	fmt.Println("  -no-cache            disable cache for this run")
	fmt.Println("  -cache-ttl 72h       override cache TTL")
	fmt.Println("  -H 'K: v'            add/override header (repeatable)")
	fmt.Println("  -q k=v               add query param (repeatable)")
	fmt.Println("cache policy: past days 30d · today 5m · counts/tasks/chats 10m · auth never")
	fmt.Println("machine readable: rayneo help --json")
}

var (
	cacheEnabled     = true
	cacheForce       = false
	cacheTTL         time.Duration
	harPath          = os.Getenv("RAYNEO_HAR")
	noRefreshFlag    = os.Getenv("RAYNEO_NO_REFRESH") != ""
	brokerURLFlag    = ""
	brokerSecretFlag = ""
)

func effectiveBroker(c *config.Config) (url, secret string) {
	url, secret = c.EffectiveBroker()
	if u := strings.TrimRight(strings.TrimSpace(brokerURLFlag), "/"); u != "" {
		url = u
	}
	if s := strings.TrimSpace(brokerSecretFlag); s != "" {
		secret = s
	}
	return url, secret
}

func hasBroker(c *config.Config) bool {
	u, s := effectiveBroker(c)
	return u != "" && s != ""
}

func brokerAutoSyncActive(c *config.Config) bool {
	return harPath == "" && os.Getenv("RAYNEO_TOKEN") == "" && hasBroker(c)
}

func loadRawConfig() *config.Config {
	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}
	return c
}

func applyHAR(c *config.Config, path string) (*har.Session, error) {
	s, err := har.Extract(path)
	if err != nil {
		return nil, err
	}
	c.Token = s.Token
	c.RefreshToken = ""
	c.NoRefresh = true
	c.TokenSource = "har:" + filepath.Base(path)
	c.TokenExpiresAt = ""
	c.BrokerETag = ""
	if !s.CapturedAt.IsZero() {
		c.TokenTime = s.CapturedAt.Format(time.RFC3339)
	} else {
		c.TokenTime = ""
	}
	if s.BaseURL != "" {
		c.BaseURL = s.BaseURL
	}
	if s.Region != "" {
		c.Region = s.Region
	}
	if s.AccountID != 0 {
		if c.AccountID != 0 && c.AccountID != s.AccountID {
			c.Account = ""
		}
		c.AccountID = s.AccountID
	}
	if s.Account != "" {
		c.Account = s.Account
	}
	if s.DeviceID != "" {
		c.DeviceID = s.DeviceID
	}
	if len(s.ClientHeaders) > 0 {
		c.ClientHeaders = s.ClientHeaders
	}
	return s, nil
}

func loadEffectiveConfig() *config.Config {
	c := loadRawConfig()
	if harPath != "" {
		if _, err := applyHAR(c, harPath); err != nil {
			fmt.Fprintln(os.Stderr, "har:", err)
			os.Exit(1)
		}
	} else if envTok := os.Getenv("RAYNEO_TOKEN"); envTok != "" {
		c.Token = envTok
		c.RefreshToken = ""
		c.NoRefresh = true
		c.TokenSource = "env:RAYNEO_TOKEN"
		c.TokenExpiresAt = ""
	}
	if noRefreshFlag {
		c.NoRefresh = true
	}
	return c
}

func maybePreSync(c *config.Config) {
	if !brokerAutoSyncActive(c) {
		return
	}
	needSync := c.Token == ""
	if !needSync {
		if expired, _, ok := broker.ParseExpiry(c.TokenExpiresAt, time.Now()); ok && expired {
			needSync = true
		}
	}
	if !needSync {
		return
	}
	bURL, bSecret := effectiveBroker(c)
	ifNoneMatch := ""
	if c.Token != "" {
		ifNoneMatch = c.BrokerETag
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resp, notMod, err := broker.Fetch(ctx, bURL, bSecret, ifNoneMatch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "broker: pre-sync failed (%v)\n", err)
		return
	}
	if notMod || resp == nil {
		return
	}
	oldTok := c.Token
	if broker.ApplyToConfig(c, resp) {
		if serr := c.Save(); serr != nil {
			fmt.Fprintf(os.Stderr, "broker: save config: %v\n", serr)
		}
	}
	if c.Token != oldTok {
		fmt.Fprintf(os.Stderr, "broker: synced updated token %s (rev %d, source %s)\n",
			mask(c.Token), resp.Revision, orDefault(resp.Source, "broker"))
	}
}

func mustConfig() *config.Config {
	c := loadEffectiveConfig()
	maybePreSync(c)
	return c
}

func printJSON(b []byte) {
	printJSONTo(os.Stdout, b)
}

func printJSONTo(w io.Writer, b []byte) {
	var v any
	if json.Unmarshal(b, &v) == nil {
		out, _ := json.MarshalIndent(v, "", "  ")
		fmt.Fprintln(w, string(out))
		return
	}
	fmt.Fprintln(w, string(b))
}

type kvFlag []string

func (k *kvFlag) String() string { return strings.Join(*k, ",") }
func (k *kvFlag) Set(v string) error {
	*k = append(*k, v)
	return nil
}

func params(kv kvFlag) url.Values {
	q := url.Values{}
	for _, it := range kv {
		if i := strings.Index(it, "="); i >= 0 {
			q.Add(it[:i], it[i+1:])
		} else {
			q.Add(it, "")
		}
	}
	return q
}

func headers(kv kvFlag) map[string]string {
	m := map[string]string{}
	for _, h := range kv {
		if i := strings.Index(h, ":"); i >= 0 {
			m[strings.TrimSpace(h[:i])] = strings.TrimSpace(h[i+1:])
		}
	}
	return m
}

func mask(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 12 {
		return "***"
	}
	return s[:6] + "…" + s[len(s)-4:]
}

func maskSecret(s string) string {
	if s == "" {
		return "(none)"
	}
	if len(s) <= 15 {
		return "***"
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

func main() {
	if len(os.Args) < 2 {
		printHelp()
		return
	}
	cmd := ""
	debug := false
	rawArgs := os.Args[1:]
	args := make([]string, 0, len(rawArgs))
	for i := 0; i < len(rawArgs); i++ {
		a := rawArgs[i]
		switch {
		case a == "-version" || a == "--version":
			fmt.Printf("rayneo %s\n", effectiveVersion())
			return
		case a == "-debug" || a == "--debug":
			debug = true
			continue
		case a == "-no-cache" || a == "--no-cache":
			cacheEnabled = false
			continue
		case a == "-refresh" || a == "--refresh" || a == "-nocache-refresh":
			cacheForce = true
			continue
		case a == "-no-refresh" || a == "--no-refresh":
			noRefreshFlag = true
			continue
		case a == "-broker-url" || a == "--broker-url":
			if i+1 >= len(rawArgs) {
				fmt.Fprintln(os.Stderr, "-broker-url needs a URL, e.g. -broker-url https://broker.example.com")
				os.Exit(2)
			}
			brokerURLFlag = rawArgs[i+1]
			i++
			continue
		case strings.HasPrefix(a, "-broker-url=") || strings.HasPrefix(a, "--broker-url="):
			brokerURLFlag = a[strings.Index(a, "=")+1:]
			continue
		case a == "-broker-secret" || a == "--broker-secret":
			if i+1 >= len(rawArgs) {
				fmt.Fprintln(os.Stderr, "-broker-secret needs a secret value")
				os.Exit(2)
			}
			brokerSecretFlag = rawArgs[i+1]
			i++
			continue
		case strings.HasPrefix(a, "-broker-secret=") || strings.HasPrefix(a, "--broker-secret="):
			brokerSecretFlag = a[strings.Index(a, "=")+1:]
			continue
		case a == "-har" || a == "--har":
			if i+1 >= len(rawArgs) {
				fmt.Fprintln(os.Stderr, "-har needs a file path, e.g. -har capture.har")
				os.Exit(2)
			}
			harPath = rawArgs[i+1]
			i++
			continue
		case strings.HasPrefix(a, "-har=") || strings.HasPrefix(a, "--har="):
			harPath = a[strings.Index(a, "=")+1:]
			continue
		case a == "-cache-ttl" || a == "--cache-ttl":
			if i+1 < len(rawArgs) {
				if d, err := time.ParseDuration(rawArgs[i+1]); err == nil {
					cacheTTL = d
					i++
					continue
				}
			}
			fmt.Fprintln(os.Stderr, "-cache-ttl needs a duration, e.g. 72h")
			os.Exit(2)
		case strings.HasPrefix(a, "-cache-ttl=") || strings.HasPrefix(a, "--cache-ttl="):
			val := a[strings.Index(a, "=")+1:]
			d, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintln(os.Stderr, "-cache-ttl needs a duration, e.g. 72h")
				os.Exit(2)
			}
			cacheTTL = d
			continue
		}
		if cmd == "" {
			if a == "-h" || a == "--help" {
				cmd = "help"
				continue
			}
			if !strings.HasPrefix(a, "-") {
				cmd = a
				continue
			}
		}
		args = append(args, a)
	}
	if cmd == "" {
		if len(args) > 0 {
			cmd = args[0]
		} else {
			printHelp()
			return
		}
	}

	switch cmd {
	case "version":
		fmt.Printf("rayneo %s\n", effectiveVersion())
	case "help", "--help", "-h", "usage":
		for _, a := range args {
			if a == "--json" || a == "-json" {
				helpJSON()
				return
			}
		}
		printHelp()
	case "sync":
		cmdSync(args, debug)
	case "har", "import-har", "from-har":
		cmdHar(args, debug)
	case "event":
		cmdEvent(args, debug)
	case "full":
		cmdFull(args, debug)
	case "recordings":
		cmdRecordings(args, debug)
	case "transcript", "tr":
		cmdTranscript(args, debug)
	case "cache":
		cmdCache(args)
	case "report":
		cmdReport(args, debug)
	case "sendcode":
		cmdSendCode(args, debug)
	case "login":
		cmdLogin(args, debug)
	case "logout":
		c := loadRawConfig()
		c.Token, c.RefreshToken, c.Account, c.AccountID = "", "", "", 0
		c.NoRefresh, c.TokenSource, c.TokenTime, c.TokenExpiresAt, c.BrokerETag, c.ClientHeaders = false, "", "", "", "", nil
		_ = c.Save()
		fmt.Println("token cleared")
	case "whoami":
		c := loadEffectiveConfig()
		src := c.TokenSource
		if src == "" {
			if c.Token != "" {
				src = "stored"
			} else {
				src = "(none)"
			}
		}
		if c.TokenTime != "" {
			src += " (" + c.TokenTime + ")"
		}
		refState := "disabled (no refreshToken)"
		if c.NoRefresh {
			refState = "disabled (no-refresh; keeps other devices logged in)"
		} else if hasBroker(c) {
			refState = "disabled (broker mode; keeps other devices logged in)"
		} else if c.RefreshToken != "" {
			refState = "auto (" + mask(c.RefreshToken) + ")"
		}
		fmt.Printf("base     : %s\nregion   : %s\naccount  : %s\naccountId: %d\ntoken    : %s\nsource   : %s\nrefresh  : %s\ndeviceId : %s\n",
			c.BaseURL, c.Region, orDash(c.Account), c.AccountID, mask(c.Token), src, refState, c.DeviceID)
		if c.TokenExpiresAt != "" {
			expLine := c.TokenExpiresAt
			if expired, rem, ok := broker.ParseExpiry(c.TokenExpiresAt, time.Now()); ok {
				if expired {
					expLine += " (expired)"
				} else {
					expLine += fmt.Sprintf(" (remaining %s)", rem.Round(time.Second))
				}
			}
			fmt.Printf("expires  : %s\n", expLine)
		}
		if bURL, bSec := effectiveBroker(c); bURL != "" || bSec != "" {
			fmt.Printf("broker   : %s (secret %s)\n", orNone(bURL), maskSecret(bSec))
		}
	case "config":
		cmdConfig(args)
	case "count", "calendar", "events", "journal", "story", "journal-detail", "jd", "suggests", "tasks", "chat":
		cmdQuery(cmd, args, debug)
	case "raw":
		cmdRaw(args, debug)
	case "refresh":
		cmdRefresh(debug)
	case "export":
		cmdExport(args, debug)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printHelp()
		os.Exit(2)
	}
}

func orDash(s string) string {
	if s == "" {
		return "(not logged in)"
	}
	return s
}

func reorderPositionalLast(args []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func cmdSync(args []string, debug bool) {
	if code := runSync(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runSync(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	urlFlag := fs.String("url", "", "token broker base URL (saved to config.json)")
	secretFlag := fs.String("secret", "", "token broker Bearer secret (saved to config.json)")
	forceFlag := fs.Bool("force", false, "skip If-None-Match ETag and force full fetch")
	verifyFlag := fs.Bool("verify", false, "verify synced token live via GET /profileapi/v1/profile/query")
	statusFlag := fs.Bool("status", false, "show broker configuration and synced token status")
	clearFlag := fs.Bool("clear", false, "clear broker_url, broker_secret, and broker_etag from config")
	if err := fs.Parse(reorderPositionalLast(args, map[string]bool{"url": true, "secret": true})); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		switch fs.Arg(0) {
		case "status":
			*statusFlag = true
		case "clear":
			*clearFlag = true
		default:
			fmt.Fprintln(stderr, "usage: rayneo sync [-url <url> -secret <secret>] [-force] [-verify] | -status | -clear")
			return 2
		}
	}

	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}

	if *statusFlag {
		bURL, bSecret := effectiveBroker(c)
		expStatus := "(unknown)"
		if expired, rem, ok := broker.ParseExpiry(c.TokenExpiresAt, time.Now()); ok {
			if expired {
				expStatus = "yes (expired)"
			} else {
				expStatus = fmt.Sprintf("no (remaining %s)", rem.Round(time.Second))
			}
		}
		refState := "disabled (no_refresh=true)"
		if !c.NoRefresh {
			if hasBroker(c) {
				refState = "disabled (broker mode)"
			} else if c.RefreshToken != "" {
				refState = "auto (" + mask(c.RefreshToken) + ")"
			} else {
				refState = "disabled (no refreshToken)"
			}
		}
		fmt.Fprintf(stdout,
			"broker    : %s\nsecret    : %s\netag      : %s\ntoken     : %s\ntokenFp   : %s\nsource    : %s\ncaptured  : %s\nexpiresAt : %s\nexpired   : %s\nrefresh   : %s\n",
			orNone(bURL), maskSecret(bSecret), orNone(c.BrokerETag), mask(c.Token),
			orNone(broker.TokenFingerprint(c.Token)), orNone(c.TokenSource),
			orNone(c.TokenTime), orNone(c.TokenExpiresAt), expStatus, refState,
		)
		return 0
	}

	if *clearFlag {
		c.BrokerURL = ""
		c.BrokerSecret = ""
		c.BrokerETag = ""
		if err := c.Save(); err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 1
		}
		fmt.Fprintln(stdout, "broker config cleared (broker_url, broker_secret, broker_etag removed)")
		return 0
	}

	saveBroker := false
	if u := strings.TrimRight(strings.TrimSpace(*urlFlag), "/"); u != "" {
		if strings.TrimRight(strings.TrimSpace(c.BrokerURL), "/") != u {
			c.BrokerETag = ""
		}
		c.BrokerURL = u
		saveBroker = true
	}
	if s := strings.TrimSpace(*secretFlag); s != "" {
		c.BrokerSecret = s
		saveBroker = true
	}

	bURL, bSecret := effectiveBroker(c)
	if u := strings.TrimRight(strings.TrimSpace(*urlFlag), "/"); u != "" {
		bURL = u
	}
	if s := strings.TrimSpace(*secretFlag); s != "" {
		bSecret = s
	}
	if bURL == "" || bSecret == "" {
		fmt.Fprintln(stderr, "sync: broker not configured — pass -url <url> -secret <secret> (or set RAYNEO_BROKER_URL and RAYNEO_BROKER_SECRET)")
		return 2
	}

	ifNoneMatch := ""
	if !*forceFlag && c.BrokerETag != "" && c.Token != "" {
		ifNoneMatch = c.BrokerETag
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	resp, notMod, err := broker.Fetch(ctx, bURL, bSecret, ifNoneMatch)
	cancel()
	if err != nil {
		if errors.Is(err, broker.ErrNoToken) {
			if saveBroker {
				c.NoRefresh = true
				c.RefreshToken = ""
				if serr := c.Save(); serr != nil {
					fmt.Fprintln(stderr, "config:", serr)
					return 1
				}
				fmt.Fprintln(stderr, "sync: broker config saved, but broker has no token synced yet — open/wake the RayNeo App on your phone (with Loon running) and retry `rayneo sync`")
				return 1
			}
			fmt.Fprintln(stderr, "sync: broker has no token synced yet — open/wake the RayNeo App on your phone (with Loon running) and retry `rayneo sync`")
			return 1
		}
		if errors.Is(err, broker.ErrUnauthorized) {
			fmt.Fprintln(stderr, "sync: broker authentication failed (401 Unauthorized) — check -secret or RAYNEO_BROKER_SECRET")
			return 1
		}
		fmt.Fprintln(stderr, "sync:", err)
		return 1
	}

	if notMod {
		needSave := saveBroker
		if !c.NoRefresh {
			c.NoRefresh = true
			needSave = true
		}
		if c.RefreshToken != "" {
			c.RefreshToken = ""
			needSave = true
		}
		if needSave {
			if serr := c.Save(); serr != nil {
				fmt.Fprintln(stderr, "config:", serr)
				return 1
			}
		}
		expInfo := orNone(c.TokenExpiresAt)
		if expired, rem, ok := broker.ParseExpiry(c.TokenExpiresAt, time.Now()); ok {
			if expired {
				expInfo += " (EXPIRED)"
			} else {
				expInfo += fmt.Sprintf(" (remaining %s)", rem.Round(time.Second))
			}
		}
		fmt.Fprintf(stdout, "unchanged (304 Not Modified) from %s:\n", bURL)
		fmt.Fprintf(stdout, "  token    : %s (fp %s)\n  source   : %s\n  expires  : %s\n  refresh  : disabled (no_refresh=true)\n",
			mask(c.Token), orNone(broker.TokenFingerprint(c.Token)), orNone(c.TokenSource), expInfo)
	} else {
		broker.ApplyToConfig(c, resp)
		if err := c.Save(); err != nil {
			fmt.Fprintln(stderr, "config:", err)
			return 1
		}
		ttlStr := formatRemaining(resp)
		fp := resp.TokenFP
		if fp == "" {
			fp = broker.TokenFingerprint(c.Token)
		}
		fmt.Fprintf(stdout, "synced from %s (rev %d, source %s):\n", bURL, resp.Revision, orDefault(resp.Source, "broker"))
		fmt.Fprintf(stdout, "  token    : %s (fp %s)\n  expires  : %s (%s)\n  base     : %s\n  region   : %s\n  accountId: %d\n  deviceId : %s\n  refresh  : disabled (no_refresh=true, refreshToken cleared; protects other devices)\n",
			mask(c.Token), orNone(fp), orNone(c.TokenExpiresAt), ttlStr,
			c.BaseURL, c.Region, c.AccountID, c.DeviceID)
		if resp.Expired {
			fmt.Fprintln(stderr, "warning: synced token is already expired — open/wake the RayNeo App on your phone (with Loon running) to sync a fresh token")
		}
	}

	if *verifyFlag {
		cl := api.New(c)
		cl.Dump = debug
		vResp, vBody, vErr := cl.Call("GET", "/profileapi/v1/profile/query", nil, nil)
		if vErr != nil {
			fmt.Fprintln(stderr, "verify error:", vErr)
			return 1
		}
		if authExpired(vBody) || vResp.StatusCode == http.StatusUnauthorized {
			fmt.Fprintf(stderr, "verify failed: HTTP %d (synced token has expired on server; open/wake the RayNeo App on your phone to sync a fresh token)\n", vResp.StatusCode)
			printJSONTo(stdout, vBody)
			return 1
		}
		if vResp.StatusCode >= 400 {
			fmt.Fprintf(stderr, "verify failed: HTTP %d\n", vResp.StatusCode)
			printJSONTo(stdout, vBody)
			return 1
		}
		fmt.Fprintf(stdout, "  verified : HTTP %d OK (/profileapi/v1/profile/query)\n", vResp.StatusCode)
	}
	return 0
}

func formatRemaining(resp *broker.TokenResponse) string {
	if resp == nil {
		return "unknown"
	}
	if resp.Expired {
		return "EXPIRED"
	}
	est := ""
	if resp.ExpiresEstimated {
		est = ", estimated"
	}
	if resp.RemainingSeconds > 0 {
		return fmt.Sprintf("remaining %s%s", (time.Duration(resp.RemainingSeconds) * time.Second).String(), est)
	}
	if expired, rem, ok := broker.ParseExpiry(resp.ExpiresAt, time.Now()); ok {
		if expired {
			return "EXPIRED"
		}
		return fmt.Sprintf("remaining %s%s", rem.Round(time.Second).String(), est)
	}
	return "unknown"
}

func cmdHar(args []string, debug bool) {
	fs := flag.NewFlagSet("har", flag.ExitOnError)
	file := fs.String("file", "", "path to .har capture file")
	fshort := fs.String("f", "", "path to .har capture file (shorthand)")
	dryRun := fs.Bool("dry-run", false, "inspect extracted token/metadata without saving to config.json")
	verify := fs.Bool("verify", false, "verify extracted token with a read-only /lifelog/api/v1/count call")
	_ = fs.Parse(reorderPositionalLast(args, map[string]bool{"file": true, "f": true}))

	target := firstNonEmpty(*file, *fshort, harPath)
	if target == "" && fs.NArg() > 0 {
		target = fs.Arg(0)
	}
	if target == "" {
		fmt.Fprintln(os.Stderr, "usage: rayneo har <file.har> [-dry-run] [-verify]")
		os.Exit(2)
	}

	c := loadRawConfig()
	s, err := applyHAR(c, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "har:", err)
		os.Exit(1)
	}

	captured := "(unknown)"
	if !s.CapturedAt.IsZero() {
		captured = s.CapturedAt.Format(time.RFC3339)
	}
	action := "imported"
	if *dryRun {
		action = "extracted (dry-run, not saved)"
	} else if err := c.Save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	fmt.Printf("%s from %s (%d entries, %d uses, %d HTTP 200, captured %s):\n",
		action, filepath.Base(target), s.EntriesTotal, s.TokenUses, s.VerifiedOK, captured)
	fmt.Printf("  base     : %s\n  region   : %s\n  accountId: %d\n  deviceId : %s\n  token    : %s\n  refresh  : disabled (no_refresh=true, refreshToken cleared; protects other devices)\n",
		c.BaseURL, c.Region, c.AccountID, c.DeviceID, mask(c.Token))

	if *verify {
		cl := api.New(c)
		cl.Dump = debug
		resp, body, err := cl.Call("GET", "/lifelog/api/v1/count", nil, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "verify error:", err)
			os.Exit(1)
		}
		if authExpired(body) {
			fmt.Fprintf(os.Stderr, "verify failed: HTTP %d (token in HAR has already expired on server)\n", resp.StatusCode)
			printJSON(body)
			os.Exit(1)
		}
		fmt.Printf("  verified : HTTP %d OK\n", resp.StatusCode)
	}
}

func cmdConfig(args []string) {
	if code := runConfig(args, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	base := fs.String("base", "", "base URL")
	region := fs.String("region", "", "cn|us|sg|eu")
	token := fs.String("token", "", "access token (disables auto-refresh)")
	harFlag := fs.String("har", "", "import access token from .har file (disables auto-refresh)")
	brokerURLOpt := fs.String("broker-url", "", "token broker URL")
	brokerSecretOpt := fs.String("broker-secret", "", "token broker Bearer secret")
	noRefresh := fs.Bool("no-refresh", false, "disable token refresh and clear stored refreshToken")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(stderr, "config:", err)
		return 1
	}
	if h := firstNonEmpty(*harFlag, harPath); h != "" {
		if _, err := applyHAR(c, h); err != nil {
			fmt.Fprintln(stderr, "har:", err)
			return 1
		}
	}
	if *base != "" {
		c.BaseURL = *base
	}
	if *region != "" {
		c.Region = *region
	}
	if *token != "" {
		c.Token = *token
		c.RefreshToken = ""
		c.NoRefresh = true
		c.TokenSource = "manual"
		c.TokenTime = time.Now().Format(time.RFC3339)
		c.TokenExpiresAt = ""
		c.BrokerETag = ""
	}
	if u := strings.TrimRight(strings.TrimSpace(firstNonEmpty(*brokerURLOpt, brokerURLFlag)), "/"); u != "" {
		if strings.TrimRight(strings.TrimSpace(c.BrokerURL), "/") != u {
			c.BrokerETag = ""
		}
		c.BrokerURL = u
	}
	if s := strings.TrimSpace(firstNonEmpty(*brokerSecretOpt, brokerSecretFlag)); s != "" {
		c.BrokerSecret = s
	}
	if *noRefresh || noRefreshFlag {
		c.NoRefresh = true
		c.RefreshToken = ""
	}
	if err := c.Save(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	bURL, bSec := effectiveBroker(c)
	if bURL != "" || bSec != "" {
		fmt.Fprintf(stdout, "saved: base=%s region=%s token=%s noRefresh=%v broker=%s brokerSecret=%s\n",
			c.BaseURL, c.Region, mask(c.Token), c.NoRefresh, orNone(bURL), maskSecret(bSec))
	} else {
		fmt.Fprintf(stdout, "saved: base=%s region=%s token=%s noRefresh=%v\n",
			c.BaseURL, c.Region, mask(c.Token), c.NoRefresh)
	}
	return 0
}

func newClient(c *config.Config, debug bool, extra map[string]string) *api.Client {
	cl := api.New(c)
	cl.Dump = debug
	cl.Extra = extra
	cl.Cache = &cache.Cache{Dir: config.CacheDir(), Enabled: cacheEnabled, Force: cacheForce, TTL: cacheTTL}
	return cl
}

func cmdSendCode(args []string, debug bool) {
	fs := flag.NewFlagSet("sendcode", flag.ExitOnError)
	account := fs.String("account", "", "phone number")
	area := fs.String("area", "+86", "area code with +")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")
	_ = fs.Parse(args)
	if *account == "" {
		fmt.Fprintln(os.Stderr, "-account is required (tip: prefer `rayneo har <file.har>` to avoid logging out other devices)")
		os.Exit(2)
	}
	payload := map[string]any{
		"sendType": 3, "bindType": 3, "type": 3,
		"loginName": *account, "bindName": *account,
		"areaCode": *area, "runtimeType": "phone",
		"validate": nil, "sceneType": 2,
	}
	cl := newClient(loadEffectiveConfig(), debug, headers(hs))
	resp, body, err := cl.Call("POST", "/identify/common/sendVerifyCode", nil, payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("HTTP %d\n", resp.StatusCode)
	printJSON(body)
}

func cmdLogin(args []string, debug bool) {
	if harPath != "" {
		cmdHar(args, debug)
		return
	}
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	account := fs.String("account", "", "phone number")
	code := fs.String("code", "", "SMS code")
	area := fs.String("area", "+86", "area code with +")
	country := fs.String("country", "cn", "country code")
	harFlag := fs.String("har", "", "import access token from .har file instead of SMS login (never refreshes)")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")
	_ = fs.Parse(args)
	if *harFlag != "" {
		harPath = *harFlag
		cmdHar(nil, debug)
		return
	}
	if *account == "" || *code == "" {
		fmt.Fprintln(os.Stderr, "prefer `rayneo har <file.har>` (or `-har <file.har>`) to avoid logging out other devices; for SMS login both -account and -code are required")
		os.Exit(2)
	}
	c := loadRawConfig()
	payload := map[string]any{
		"code": *code, "deviceId": c.DeviceID,
		"sendType": 3, "bindType": 3, "type": 3,
		"loginName": *account, "bindName": *account,
		"areaCode": *area, "runtimeType": "phone",
		"countryCode":   *country,
		"systemVersion": nil, "deviceModel": nil, "language": nil,
		"source": nil, "ageCheck": nil,
	}
	cl := newClient(c, debug, headers(hs))
	resp, body, err := cl.Call("POST", "/identify/quickLogin", nil, payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("HTTP %d\n", resp.StatusCode)

	var r struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
			UserInfo     struct {
				AccountID int64  `json:"accountId"`
				NickName  string `json:"nickName"`
			} `json:"userInfo"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		printJSON(body)
		return
	}
	if r.Data.Token == "" {
		printJSON(body)
		fmt.Fprintln(os.Stderr, "login failed")
		os.Exit(1)
	}
	c.Token = r.Data.Token
	if noRefreshFlag {
		c.RefreshToken = ""
		c.NoRefresh = true
	} else {
		c.RefreshToken = r.Data.RefreshToken
		c.NoRefresh = false
	}
	c.TokenSource = "login"
	c.TokenTime = time.Now().Format(time.RFC3339)
	c.TokenExpiresAt = ""
	c.BrokerETag = ""
	c.ClientHeaders = nil
	c.Account, c.AccountID = *account, r.Data.UserInfo.AccountID
	if err := c.Save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Printf("logged in: accountId=%d nick=%s expiresIn=%ds token=%s\n",
		r.Data.UserInfo.AccountID, r.Data.UserInfo.NickName, r.Data.ExpiresIn, mask(r.Data.Token))
}

// cmdRefresh renews the access token.
// The refresh token must be sent in the `xtcl-access-token` header with an empty
// body. The previous access token is invalidated; the refresh token is reused.
func cmdRefresh(debug bool) {
	c := loadEffectiveConfig()
	if err := doRefresh(c, debug); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func doRefresh(c *config.Config, debug bool) error {
	if c.NoRefresh || noRefreshFlag || harPath != "" || hasBroker(c) {
		return fmt.Errorf("token refresh is disabled (no-refresh / broker / HAR mode) to avoid invalidating logins on other devices; use `rayneo sync` or `rayneo har <file.har>`")
	}
	if c.RefreshToken == "" {
		return fmt.Errorf("no refreshToken stored; run `rayneo sync`, `rayneo har <file.har>`, or `rayneo login`")
	}
	cl := newClient(c, debug, map[string]string{"xtcl-access-token": c.RefreshToken})
	resp, body, err := cl.Call("POST", "/authapi/token/refresh", nil, nil)
	if err != nil {
		return err
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token        string `json:"token"`
			RefreshToken string `json:"refreshToken"`
			ExpiresIn    int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Data.Token == "" {
		return fmt.Errorf("refresh failed: HTTP %d code=%d msg=%s", resp.StatusCode, r.Code, r.Msg)
	}
	c.Token = r.Data.Token
	if r.Data.RefreshToken != "" {
		c.RefreshToken = r.Data.RefreshToken
	}
	c.TokenTime = time.Now().Format(time.RFC3339)
	if err := c.Save(); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	fmt.Printf("refreshed: token=%s expiresIn=%ds\n", mask(c.Token), r.Data.ExpiresIn)
	return nil
}

// authExpired reports whether the body is a token failure.
func authExpired(body []byte) bool {
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	if r.Code != 401 {
		return false
	}
	return strings.Contains(strings.ToLower(r.Msg), "token") ||
		strings.Contains(strings.ToLower(r.Msg), "authentication")
}

func warnAuthExpired(c *config.Config) {
	switch {
	case c.Token == "":
		fmt.Fprintln(os.Stderr, "auth: missing token — prefer `rayneo sync` (Token Broker) or `rayneo har <file.har>` to authenticate without logging out other devices")
	case c.NoRefresh || noRefreshFlag || harPath != "":
		fmt.Fprintln(os.Stderr, "auth: access token expired or invalid (auto-refresh is disabled to protect other devices; run `rayneo sync` or re-capture with `rayneo har <file.har>`)")
	default:
		fmt.Fprintln(os.Stderr, "auth: access token expired or invalid (prefer `rayneo sync` or `rayneo har <file.har>` to update token without logging out other devices)")
	}
}

func callWithAuth(cl *api.Client, method, path string, q url.Values, payload any) (*http.Response, []byte, error) {
	resp, body, err := cl.Call(method, path, q, payload)
	if err != nil {
		return nil, nil, err
	}
	if authExpired(body) {
		if brokerAutoSyncActive(cl.Cfg) {
			failedTok := cl.Cfg.Token
			bURL, bSecret := effectiveBroker(cl.Cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			bResp, _, bErr := broker.Fetch(ctx, bURL, bSecret, "")
			cancel()
			if bErr != nil {
				fmt.Fprintf(os.Stderr, "auth: token expired and broker sync failed (%v) — open/wake the RayNeo App on your phone (with Loon running) to sync a fresh token\n", bErr)
				return resp, body, nil
			}
			if bResp != nil && strings.TrimSpace(bResp.AccessToken) != "" && strings.TrimSpace(bResp.AccessToken) != failedTok {
				broker.ApplyToConfig(cl.Cfg, bResp)
				if serr := cl.Cfg.Save(); serr != nil {
					fmt.Fprintf(os.Stderr, "broker: save config: %v\n", serr)
				}
				fmt.Fprintf(os.Stderr, "broker: token expired, synced fresh token %s (rev %d) and retrying\n", mask(cl.Cfg.Token), bResp.Revision)
				retryResp, retryBody, retryErr := cl.Call(method, path, q, payload)
				if retryErr != nil {
					return nil, nil, retryErr
				}
				if authExpired(retryBody) {
					fmt.Fprintln(os.Stderr, "auth: freshly synced broker token was also rejected (401) — open/wake the RayNeo App on your phone (with Loon running) to trigger a fresh token sync")
				}
				return retryResp, retryBody, nil
			}
			fmt.Fprintln(os.Stderr, "auth: access token expired and broker still has the same token — open/wake the RayNeo App on your phone (with Loon running) to sync a fresh token, then retry")
			return resp, body, nil
		}
		if cl.Cfg.CanRefresh() && !noRefreshFlag && harPath == "" {
			if rerr := doRefresh(cl.Cfg, false); rerr == nil {
				return cl.Call(method, path, q, payload)
			}
		}
		warnAuthExpired(cl.Cfg)
	}
	return resp, body, nil
}

func cmdQuery(name string, args []string, debug bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	from := fs.String("from", "", "YYYY-MM")
	to := fs.String("to", "", "YYYY-MM")
	date := fs.String("date", "", "YYYY-MM-DD")
	tz := fs.String("tz", "Asia/Shanghai", "timezone")
	_ = fs.Parse(args)

	q := params(kv)
	c := mustConfig()
	cl := newClient(c, debug, headers(hs))

	switch name {
	case "count":
		call(cl, "GET", "/lifelog/api/v1/count", q)
	case "calendar":
		payload := map[string]any{}
		if *from != "" && *to != "" {
			payload["startYearMonth"] = ym(*from)
			payload["endYearMonth"] = ym(*to)
		} else {
			now := time.Now()
			payload["startYearMonth"] = now.Year()*100 + int(now.Month())
			payload["endYearMonth"] = now.AddDate(0, 1, 0).Year()*100 + int(now.AddDate(0, 1, 0).Month())
		}
		callBody(cl, "POST", "/lifelog/api/v1/calendar", q, payload)
	case "events":
		if *date != "" {
			q.Set("date", *date)
		}
		q.Set("timezone", *tz)
		call(cl, "GET", "/lifelog/api/v1/events", q)
	case "journal", "story":
		if *date != "" {
			q.Set("date", *date)
		}
		call(cl, "GET", "/lifelog/api/v1/journal/latest", q)
	case "journal-detail", "jd":
		if *date == "" {
			*date = time.Now().Format("2006-01-02")
		}
		callBody(cl, "POST", "/lifelog/api/v1/journal/detail", q,
			map[string]any{"journalDate": *date})
	case "suggests":
		callBody(cl, "POST", "/summary-service/suggest/list", q, map[string]any{})
	case "tasks":
		q.Set("accountId", fmt.Sprintf("%d", c.AccountID))
		q.Set("sources", "rayneoTasks")
		call(cl, "GET", "/action/api/v2/tasks", q)
	case "chat":
		q.Set("pageSize", "30")
		call(cl, "GET", "/clsapi/aiChat/selectPage", q)
	}
}

func ym(s string) int {
	var y, m int
	if _, err := fmt.Sscanf(s, "%d-%d", &y, &m); err != nil {
		return 0
	}
	return y*100 + m
}

func call(cl *api.Client, method, path string, q url.Values) {
	callBody(cl, method, path, q, nil)
}

func callBody(cl *api.Client, method, path string, q url.Values, payload any) {
	resp, body, err := callWithAuth(cl, method, path, q, payload)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cl.Dump {
		mark := ""
		if cl.Hit {
			mark = " (cache hit)"
		}
		fmt.Fprintf(os.Stderr, "HTTP %d%s\n", resp.StatusCode, mark)
	} else if cl.Hit && os.Getenv("RAYNEO_VERBOSE") != "" {
		fmt.Fprintln(os.Stderr, "[cache hit]")
	}
	printJSON(body)
	if authExpired(body) {
		os.Exit(1)
	}
}

func cmdRaw(args []string, debug bool) {
	fs := flag.NewFlagSet("raw", flag.ExitOnError)
	method := fs.String("X", "GET", "HTTP method")
	path := fs.String("p", "", "path, e.g. /lifelog/api/v1/events")
	data := fs.String("d", "", "JSON body")
	var kv, hs kvFlag
	form := fs.Bool("f", false, "send -q pairs as an urlencoded body instead of query")
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	_ = fs.Parse(args)
	if *path == "" {
		fmt.Fprintln(os.Stderr, "-p is required")
		os.Exit(2)
	}
	var payload any
	if *data != "" {
		if err := json.Unmarshal([]byte(*data), &payload); err != nil {
			fmt.Fprintln(os.Stderr, "bad -d JSON:", err)
			os.Exit(2)
		}
	}
	cl := newClient(mustConfig(), debug, headers(hs))
	q := params(kv)
	if *form {
		cl.Form = q.Encode()
		q = nil
	}
	callBody(cl, strings.ToUpper(*method), *path, q, payload)
}

func cmdExport(args []string, debug bool) {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	from := fs.String("from", "", "YYYY-MM-DD")
	to := fs.String("to", "", "YYYY-MM-DD")
	out := fs.String("out", "rayneo-export", "output dir")
	format := fs.String("format", "all", "json|csv|md|all")
	_ = fs.Parse(args)
	if *from == "" || *to == "" {
		fmt.Fprintln(os.Stderr, "-from and -to are required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	want := func(k string) bool { return *format == "all" || *format == k }

	c := mustConfig()
	cl := newClient(c, debug, nil)
	days := dayRange(*from, *to)
	fmt.Printf("exporting %d day(s) -> %s (format=%s)\n", len(days), *out, *format)

	for _, d := range days {
		q := url.Values{"date": {d}, "timezone": {"Asia/Shanghai"}}
		_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events", q, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "events %s: %v\n", d, err)
		} else if authExpired(body) {
			os.Exit(1)
		} else {
			if want("json") {
				writeFile(filepath.Join(*out, "events-"+d+".json"), body)
			}
			if want("csv") {
				rows := eventsFromBody(body, d)
				if len(rows) > 0 {
					_ = writeEventsCSV(filepath.Join(*out, "events-"+d+".csv"), rows)
				}
			}
		}
		time.Sleep(400 * time.Millisecond)

		_, jb, err := callWithAuth(cl, "POST", "/lifelog/api/v1/journal/detail", nil,
			map[string]any{"journalDate": d})
		if err != nil {
			fmt.Fprintf(os.Stderr, "journal %s: %v\n", d, err)
		} else if authExpired(jb) {
			os.Exit(1)
		} else if j := journalFromBody(jb); j != nil && (j.Context != "" || len(j.Sections) > 0) {
			if want("json") {
				writeFile(filepath.Join(*out, "journal-"+d+".json"), jb)
			}
			if want("md") {
				_ = os.WriteFile(filepath.Join(*out, "journal-"+d+".md"), []byte(j.Markdown()), 0o644)
			}
			fmt.Printf("  %s  ok (events + journal)\n", d)
		} else {
			fmt.Printf("  %s  no journal\n", d)
		}
		time.Sleep(400 * time.Millisecond)
	}
	fmt.Println("done:", *out)
}

func cmdFull(args []string, debug bool) {
	fs := flag.NewFlagSet("full", flag.ExitOnError)
	id := fs.String("id", "", "event id")
	date := fs.String("date", "", "YYYY-MM-DD: all events of that day")
	format := fs.String("format", "md", "md|txt|srt|json")
	out := fs.String("out", "", "write files into this dir")
	tz := fs.String("tz", "Asia/Shanghai", "timezone for -date")
	_ = fs.Parse(args)

	cl := newClient(mustConfig(), debug, nil)
	var ids []string
	labels := map[string]string{}
	switch {
	case *id != "":
		ids = []string{*id}
	case *date != "":
		q := url.Values{"date": {*date}, "timezone": {*tz}}
		_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events", q, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if authExpired(body) {
			os.Exit(1)
		}
		var r struct {
			Data struct {
				Events []map[string]any `json:"events"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &r)
		for _, e := range r.Data.Events {
			eid := asString(e["id"])
			if eid == "" {
				continue
			}
			ids = append(ids, eid)
			labels[eid] = sanitize(asString(e["title"]))
		}
	default:
		fmt.Fprintln(os.Stderr, "need -id <eventId> or -date YYYY-MM-DD")
		os.Exit(2)
	}

	total := 0
	for i, eid := range ids {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		f, err := fetchFull(cl, eid)
		if err != nil {
			// recording-type ids are not lifelog events; fall back to the aiTask store
			if tb, terr := fetchTranscript(cl, eid); terr == nil {
				total += len(tb.Recording.Clips)
				if *out != "" {
					name := sanitize(fmt.Sprintf("rec_%d_%s", tb.AITask.CreateTime, tb.AITask.Name))
					if err := writeTranscriptDir(*out, tb, name); err == nil {
						fmt.Printf("  %s  %4d 段  %s (recording)\n", eid, len(tb.Recording.Clips), name)
					}
				}
				continue
			}
			fmt.Fprintf(os.Stderr, "  %s: %v\n", eid, err)
			continue
		}
		total += f.allClips()
		if *out != "" {
			shortID := eid
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			name := sanitize(fmt.Sprintf("%s_%s_%s", strings.ReplaceAll(f.StartTimeFormatted, ":", ""), shortID, firstNonEmpty(labels[eid], f.Title)))
			if err := writeFullDir(*out, f, name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			fmt.Printf("  %s  %4d 段  %s\n", eid, f.allClips(), name)
			continue
		}
		switch *format {
		case "txt":
			fmt.Print(f.Text())
		case "srt":
			fmt.Print(f.SRT())
		case "json":
			j, _ := json.MarshalIndent(f, "", "  ")
			fmt.Println(string(j))
		default:
			fmt.Print(f.Markdown())
		}
	}
	if *out != "" {
		fmt.Printf("\n共 %d 个事件 / %d 段转录\n", len(ids), total)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func cmdEvent(args []string, debug bool) {
	fs := flag.NewFlagSet("event", flag.ExitOnError)
	id := fs.String("id", "", "event id (from `events -date`)")
	raw := fs.Bool("raw", false, "print the raw JSON instead of a digest")
	_ = fs.Parse(args)
	if *id == "" {
		fmt.Fprintln(os.Stderr, "-id is required")
		os.Exit(2)
	}
	cl := newClient(mustConfig(), debug, nil)
	_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events/"+*id, nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if authExpired(body) {
		printJSON(body)
		os.Exit(1)
	}
	if *raw {
		printJSON(body)
		return
	}
	var r struct {
		Data map[string]any `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil || r.Data == nil {
		printJSON(body)
		return
	}
	d := r.Data
	fmt.Printf("# %s\n\n", asString(d["title"]))
	fmt.Printf("%s-%s · %s · %v 个参与者\n\n",
		asString(d["startTimeFormatted"]), asString(d["endTimeFormatted"]),
		asString(d["audioLengthFormatted"]), d["participantCount"])
	if s := asString(d["summaryContent"]); s != "" {
		fmt.Printf("## 摘要\n\n%s\n\n", s)
	}
	if ol, ok := d["outline"].([]any); ok && len(ol) > 0 {
		fmt.Printf("## 要点（%d）\n\n", len(ol))
		for _, o := range ol {
			m, _ := o.(map[string]any)
			if m == nil {
				continue
			}
			ts := int64(0)
			if f, ok := m["timestamp"].(float64); ok {
				ts = int64(f)
			}
			when := ""
			if ts > 0 {
				when = time.UnixMilli(ts).Format("15:04") + " "
			}
			fmt.Printf("- %s%s\n", when, asString(m["content"]))
		}
		fmt.Println()
	}
	if ogl, ok := d["originalList"].([]any); ok && len(ogl) > 0 {
		fmt.Printf("## 原始子事件（%d）\n\n", len(ogl))
		for _, o := range ogl {
			m, _ := o.(map[string]any)
			if m == nil {
				continue
			}
			fmt.Printf("- %s-%s · %s · %s\n", asString(m["startTimeFormatted"]),
				asString(m["endTimeFormatted"]), asString(m["eventType"]), asString(m["title"]))
		}
		fmt.Println()
	}
	if ti, ok := d["transcriptionInfos"].([]any); ok && len(ti) > 0 {
		fmt.Printf("## 转录来源\n\n")
		for _, t := range ti {
			m, _ := t.(map[string]any)
			fmt.Printf("- `%s` (%s)\n", asString(m["sourceId"]), asString(m["sourceType"]))
		}
	}
}

func cmdRecordings(args []string, debug bool) {
	fs := flag.NewFlagSet("recordings", flag.ExitOnError)
	typ := fs.String("type", "recording", "taskType: recording|cc|'' for all")
	n := fs.Int("n", 50, "page size")
	_ = fs.Parse(args)
	cl := newClient(mustConfig(), debug, nil)
	rows, err := listAITasks(cl, *typ, *n)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("%-34s %-10s %-9s %-8s %s\n", "TASKID", "TYPE", "DURATION", "WHEN", "NAME")
	for _, r := range rows {
		dur := ""
		if r.DurationMs > 0 {
			dur = fmt.Sprintf("%d:%02d", r.DurationMs/60000, (r.DurationMs/1000)%60)
		}
		when := time.UnixMilli(r.CreateTime).Format("01-02 15:04")
		fmt.Printf("%-34s %-10s %-9s %-8s %s\n", r.TaskID, r.TaskType, dur, when, r.Name)
	}
	fmt.Printf("\n共 %d 条\n", len(rows))
}

func cmdTranscript(args []string, debug bool) {
	fs := flag.NewFlagSet("transcript", flag.ExitOnError)
	id := fs.String("id", "", "taskId (event id)")
	date := fs.String("date", "", "YYYY-MM-DD: pull transcripts for all events of that day")
	format := fs.String("format", "md", "md|txt|srt|json")
	out := fs.String("out", "", "write files into this dir instead of stdout")
	tz := fs.String("tz", "Asia/Shanghai", "timezone for -date")
	_ = fs.Parse(args)

	c := mustConfig()
	cl := newClient(c, debug, nil)

	var ids []string
	names := map[string]string{}
	switch {
	case *id != "":
		ids = append(ids, *id)
	case *date != "":
		q := url.Values{"date": {*date}, "timezone": {*tz}}
		_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events", q, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if authExpired(body) {
			os.Exit(1)
		}
		var r struct {
			Data struct {
				Events []map[string]any `json:"events"`
			} `json:"data"`
		}
		_ = json.Unmarshal(body, &r)
		for _, e := range r.Data.Events {
			tid := asString(e["id"])
			if tid == "" {
				continue
			}
			ids = append(ids, tid)
			names[tid] = sanitize(asString(e["title"]))
		}
	default:
		fmt.Fprintln(os.Stderr, "need -id <taskId> or -date YYYY-MM-DD")
		os.Exit(2)
	}

	for i, tid := range ids {
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		b, err := fetchTranscript(cl, tid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", tid, err)
			continue
		}
		label := names[tid]
		if label == "" {
			label = sanitize(b.AITask.Name)
		}
		if label == "" {
			label = tid
		}
		if *out != "" {
			name := fmt.Sprintf("%s_%s", time.UnixMilli(b.AITask.CreateTime).Format("2006-01-02_1504"), label)
			if err := writeTranscriptDir(*out, b, name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			fmt.Printf("  %s -> %s.{md,txt,srt,json}\n", tid, filepath.Join(*out, name))
			continue
		}
		switch *format {
		case "txt":
			fmt.Print(b.Text())
		case "srt":
			fmt.Print(b.SRT())
		case "json":
			j, _ := json.MarshalIndent(fOrBundleJSON(b), "", "  ")
			fmt.Println(string(j))
		default:
			fmt.Print(b.Markdown())
		}
	}
}

func fOrBundleJSON(b *transcriptBundle) *transcriptBundle { return b }

func sanitize(s string) string {
	bad := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\t"}
	for _, b := range bad {
		s = strings.ReplaceAll(s, b, "_")
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) > 40 {
		s = string([]rune(s)[:40])
	}
	return s
}

func cmdCache(args []string) {
	sub := "stat"
	if len(args) > 0 {
		sub = args[0]
	}
	c := &cache.Cache{Dir: config.CacheDir(), Enabled: cacheEnabled, Force: cacheForce, TTL: cacheTTL}
	switch sub {
	case "clear", "rm":
		if err := c.Clear(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("cache cleared:", c.Dir)
	case "ls", "list":
		for _, e := range c.List(20) {
			age := time.Since(time.Unix(e.FetchedAt, 0)).Round(time.Second)
			ttl := time.Duration(e.TTL) * time.Second
			state := "fresh"
			if age > ttl {
				state = "stale"
			}
			q := e.Query
			if len(q) > 40 {
				q = q[:40] + "…"
			}
			fmt.Printf("%-6s %-8s %-46s %s\n", state, age, e.Path, q)
		}
	case "stat":
		files, bytes, fresh, stale := c.Stat()
		fmt.Printf("dir   : %s\nfiles : %d (%.1f KB)\nfresh : %d\nstale : %d\n",
			c.Dir, files, float64(bytes)/1024, fresh, stale)
	default:
		fmt.Fprintln(os.Stderr, "usage: rayneo cache [stat|ls|clear]")
		os.Exit(2)
	}
}

func cmdReport(args []string, debug bool) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	dir := fs.String("dir", "rayneo-export", "export dir")
	out := fs.String("out", "report.md", "output file")
	title := fs.String("title", "", "report title")
	_ = fs.Parse(args)
	if err := buildReport(*dir, *out, *title); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("report written:", *out)
}

func writeFile(p string, b []byte) {
	var v any
	if json.Unmarshal(b, &v) == nil {
		if out, err := json.MarshalIndent(v, "", "  "); err == nil {
			b = out
		}
	}
	_ = os.WriteFile(p, b, 0o644)
}

func dayRange(from, to string) []string {
	var y1, m1, d1, y2, m2, d2 int
	fmt.Sscanf(from, "%d-%d-%d", &y1, &m1, &d1)
	fmt.Sscanf(to, "%d-%d-%d", &y2, &m2, &d2)
	t := time.Date(y1, time.Month(m1), d1, 0, 0, 0, 0, time.UTC)
	end := time.Date(y2, time.Month(m2), d2, 0, 0, 0, 0, time.UTC)
	var out []string
	for !t.After(end) && len(out) < 3700 {
		out = append(out, t.Format("2006-01-02"))
		t = t.AddDate(0, 0, 1)
	}
	return out
}
