package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ---------- event / journal views (raw JSON stays available) ----------

type eventRow struct {
	Date         string
	Start        string
	End          string
	Duration     string
	Type         string
	Flow         string
	Title        string
	Participants string
	Summary      string
	ID           string
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// eventsFromBody extracts event rows from the /lifelog/api/v1/events response.
func eventsFromBody(body []byte, date string) []eventRow {
	var r struct {
		Data struct {
			Date   string           `json:"date"`
			Events []map[string]any `json:"events"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	if r.Data.Date != "" {
		date = r.Data.Date
	}
	rows := make([]eventRow, 0, len(r.Data.Events))
	for _, e := range r.Data.Events {
		parts := ""
		if p, ok := e["participants"].([]any); ok {
			ss := make([]string, 0, len(p))
			for _, x := range p {
				ss = append(ss, asString(x))
			}
			parts = strings.Join(ss, " ")
		}
		dur := asString(e["audioLengthFormatted"])
		if dur == "" {
			dur = fmt.Sprintf("%vs", asString(e["durationSeconds"]))
		}
		rows = append(rows, eventRow{
			Date:         date,
			Start:        asString(e["startTimeFormatted"]),
			End:          asString(e["endTimeFormatted"]),
			Duration:     dur,
			Type:         asString(e["eventType"]),
			Flow:         asString(e["flowType"]),
			Title:        asString(e["title"]),
			Participants: parts,
			Summary:      asString(e["summaryContent"]),
			ID:           asString(e["id"]),
		})
	}
	return rows
}

func writeEventsCSV(path string, rows []eventRow) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString("\ufeff"); err != nil { // BOM so Excel reads UTF-8
		return err
	}
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{"date", "start", "end", "duration", "type", "flow", "title", "participants", "summary", "id"})
	for _, r := range rows {
		_ = w.Write([]string{r.Date, r.Start, r.End, r.Duration, r.Type, r.Flow, r.Title, r.Participants, r.Summary, r.ID})
	}
	return w.Error()
}

// ---------- journal -> markdown ----------

type sectionView struct {
	Title  string
	Blocks []string
}

type journalView struct {
	Title       string
	PeriodStart string
	PeriodEnd   string
	Mode        string
	VisualTag   string
	GeneratedAt string
	Context     string
	Counts      string
	Sections    []sectionView
}

func journalFromBody(body []byte) *journalView {
	var r struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal(r.Data, &obj) != nil {
		var arr []map[string]any
		if json.Unmarshal(r.Data, &arr) != nil || len(arr) == 0 {
			return nil
		}
		obj = arr[0]
	}
	counts := []string{}
	if cd, ok := obj["countData"].([]any); ok {
		for _, c := range cd {
			m, _ := c.(map[string]any)
			if m == nil {
				continue
			}
			counts = append(counts, fmt.Sprintf("%s %v", asString(m["type"]), m["count"]))
		}
	}
	gen := asString(obj["generatedAt"])
	if len(gen) > 19 {
		gen = gen[:19]
	}
	var sections []sectionView
	if raw, ok := obj["sections"].([]any); ok {
		for _, sv := range raw {
			m, _ := sv.(map[string]any)
			if m == nil {
				continue
			}
			sec := sectionView{Title: asString(m["title"])}
			if bs, ok := m["blocks"].([]any); ok {
				for _, bv := range bs {
					bm, _ := bv.(map[string]any)
					if bm == nil {
						continue
					}
					if c := asString(bm["content"]); c != "" {
						sec.Blocks = append(sec.Blocks, c)
					}
				}
			}
			if sec.Title != "" || len(sec.Blocks) > 0 {
				sections = append(sections, sec)
			}
		}
	}
	return &journalView{
		Title:       asString(obj["title"]),
		PeriodStart: asString(obj["periodStart"]),
		PeriodEnd:   asString(obj["periodEnd"]),
		Mode:        asString(obj["mode"]),
		VisualTag:   asString(obj["visualTag"]),
		GeneratedAt: gen,
		Context:     asString(obj["context"]),
		Counts:      strings.Join(counts, " · "),
		Sections:    sections,
	}
}

func (j *journalView) Markdown() string {
	var b strings.Builder
	title := j.Title
	if title == "" {
		title = j.PeriodStart + " 每日故事"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	meta := []string{}
	if j.PeriodStart != "" {
		meta = append(meta, fmt.Sprintf("**日期** %s", j.PeriodStart))
	}
	if j.VisualTag != "" {
		meta = append(meta, fmt.Sprintf("**情绪** %s", j.VisualTag))
	}
	if j.Mode != "" {
		meta = append(meta, fmt.Sprintf("**模式** %s", j.Mode))
	}
	if j.Counts != "" {
		meta = append(meta, fmt.Sprintf("**统计** %s", j.Counts))
	}
	if j.GeneratedAt != "" {
		meta = append(meta, fmt.Sprintf("**生成于** %s", j.GeneratedAt))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n\n")
	}
	if j.Context != "" {
		b.WriteString(j.Context + "\n")
	}
	for _, sec := range j.Sections {
		fmt.Fprintf(&b, "\n## %s\n\n", sec.Title)
		for _, blk := range sec.Blocks {
			b.WriteString(blk + "\n\n")
		}
	}
	if j.Context == "" && len(j.Sections) == 0 {
		b.WriteString("_（该日期没有生成日记内容）_\n")
	}
	return b.String()
}

// ---------- report: aggregate a directory of exports ----------

func buildReport(dir, out string, title string) error {
	jfiles, _ := filepath.Glob(filepath.Join(dir, "journal-*.md"))
	sort.Strings(jfiles)
	if len(jfiles) == 0 {
		return fmt.Errorf("no journal-*.md in %s (run export first)", dir)
	}
	var b strings.Builder
	if title == "" {
		title = "RayNeo AI 全天智记 · 汇总"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	fmt.Fprintf(&b, "_由 rayneo-cli 从 %d 篇日记聚合生成_\n\n", len(jfiles))

	type reportEntry struct {
		date  string
		title string
		rest  string
	}
	entries := make([]reportEntry, 0, len(jfiles))
	for _, f := range jfiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		base := strings.TrimSuffix(filepath.Base(f), ".md")
		date := strings.TrimPrefix(base, "journal-")
		lines := strings.SplitN(string(data), "\n", 2)
		head := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[0]), "#"))
		// Avoid duplicating default "<date> 每日故事" if it just echoes the date.
		if head == date+" 每日故事" || head == date {
			head = ""
		}
		rest := ""
		if len(lines) > 1 {
			rest = strings.TrimSpace(lines[1])
		}
		entries = append(entries, reportEntry{date: date, title: head, rest: rest})
	}

	b.WriteString("## 目录\n\n")
	for _, e := range entries {
		label := e.date
		if e.title != "" {
			label += " · " + e.title
		}
		fmt.Fprintf(&b, "- [%s](#%s)\n", label, strings.ReplaceAll(e.date, ".", ""))
	}
	b.WriteString("\n---\n\n")

	for _, e := range entries {
		heading := e.date
		if e.title != "" {
			heading += " · " + e.title
		}
		fmt.Fprintf(&b, "<a id=\"%s\"></a>\n\n", strings.ReplaceAll(e.date, ".", ""))
		fmt.Fprintf(&b, "## %s\n\n", heading)
		if e.rest != "" {
			b.WriteString(e.rest)
			b.WriteString("\n\n---\n\n")
		}
	}
	return os.WriteFile(out, []byte(b.String()), 0o644)
}

// ---------- help spec (machine readable) ----------

type argSpec struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Default string `json:"default,omitempty"`
	Desc    string `json:"desc"`
}

type cmdSpec struct {
	Name                 string    `json:"name"`
	Summary              string    `json:"summary"`
	Method               string    `json:"http_method,omitempty"`
	Path                 string    `json:"http_path,omitempty"`
	Args                 []argSpec `json:"args,omitempty"`
	Example              string    `json:"example,omitempty"`
	Output               string    `json:"output,omitempty"`
	ReadOnly             bool      `json:"read_only"`
	RequiresConfirmation bool      `json:"requires_confirmation,omitempty"`
}

var helpSpec = struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	Auth        string    `json:"auth"`
	Safety      string    `json:"safety"`
	Notes       []string  `json:"notes"`
	Commands    []cmdSpec `json:"commands"`
}{
	Name:        "rayneo",
	Version:     "dev",
	Description: "Query, export, and manage RayNeo AI lifelog (全天智记) data: events, calendar, AI daily journal, GPS trails, Obsidian daily digests, full transcripts, AI chat histories, facts, devices, profile, membership quota, and task management (CRUD). Prefer passive Token Broker sync (`rayneo sync`, Scheme A) or HAR token mode (`rayneo har <file.har>` / `-har <file.har>`) so mobile/other devices stay logged in; every request is signed with TCL1-HMAC-SHA256.",
	Auth:        "[PREFERRED / 自动同步] `rayneo sync -url <broker_url> -secret <secret> -verify` (Scheme A: passive Token Broker + iOS Loon plugin): automatically mirrors the mobile App's active `access_token` with `no_refresh=true` and transparent 401 auto-recovery, never kicking mobile/other devices offline.\n      [PREFERRED / 手动抓包] `rayneo har <capture.har> -verify` (or one-off `-har <capture.har>` / `RAYNEO_HAR=<capture.har>`): extracts `xtcl-access-token` from packet capture with refresh disabled.\n      [FALLBACK  / 备选慎用] `sendcode -account <phone>` -> `login -account <phone> -code <code>`: SMS login invalidates other devices' refreshToken, and `/authapi/token/refresh` immediately invalidates the active access token.",
	Safety:      "PERMISSION REQUIRED FOR ALL NON-READ-ONLY COMMANDS: Any command with `read_only: false` (`requires_confirmation: true`) — including `tasks add|done|reopen|update|delete`, `raw`, `sync`, `har`, `config`, `logout`, `sendcode`, `login`, and `refresh` — MUST NOT be executed by an AI agent or automated workflow without explicit user permission (only read-only forms like `tasks [list]`, `sync -status`, and `har -dry-run` may run without write confirmation).",
	Notes: []string{
		"NON-READ-ONLY PERMISSION RULE (CRITICAL): Every command marked `read_only: false` / `requires_confirmation: true` mutates remote cloud data (`tasks add/done/reopen/update/delete`, non-GET `raw`), invalidates sessions (`sendcode`, `login`, `refresh`), or modifies local credentials (`sync`, `har`, `config`, `logout`). Always obtain explicit user approval before running any non-read-only command, showing the exact command and target IDs/parameters first.",
		"AUTH PRIORITY: Always prefer `rayneo sync` (Scheme A Token Broker) or `rayneo har <file.har>` (or `-har <file.har>`) over `sendcode`/`login`/`refresh`. Re-logging in via SMS invalidates the phone App's refreshToken, and refreshing invalidates its current access token.",
		"All query/transcript/trail/digest/account commands are strictly read-only. Only `tasks` write subcommands (`add`/`create`, `done`/`complete`, `reopen`/`undone`, `update`/`edit`, `delete`/`rm`) mutate remote task data and require explicit user permission.",
		"Date flags on events, jd, full, transcript, trail, digest, and export accept `today`, `yesterday`, or `-Nd` (e.g. `-7d`) in addition to `YYYY-MM-DD`.",
		"Broker mode (`rayneo sync`, or global `-broker-url` / `-broker-secret` / `RAYNEO_BROKER_URL` / `RAYNEO_BROKER_SECRET`) pulls the passive access_token mirror with `no_refresh=true` and `refresh_token` cleared. When `-har` is not active and broker is configured, commands auto-sync before requests if token is missing/expired and transparently retry once on 401 if the broker has a fresher token.",
		"HAR mode (`rayneo har <file.har>` or global `-har <file.har>` / `RAYNEO_HAR`) extracts `xtcl-access-token` and safe client headers while clearing `refreshToken` and setting `no_refresh=true`.",
		"-no-refresh disables token renewal even if a refreshToken is present.",
		"-version prints `rayneo <version>` and exits.",
		"-debug prints the canonical string, stringToSign and Authorization header for each request.",
		"-H 'Name: value' adds/overrides a raw header; -q k=v adds a query param (both repeatable).",
		"Optional env overrides: RAYNEO_BROKER_URL, RAYNEO_BROKER_SECRET, RAYNEO_HAR, RAYNEO_TOKEN, RAYNEO_NO_REFRESH, RAYNEO_APP_ID, RAYNEO_APP_KEY.",
		"JSON is printed pretty to stdout; exit code 1 on transport/auth failure.",
		"Rate: keep it gentle; use export with its built-in 400ms spacing for bulk pulls.",
		"Transcripts live in two stores: /lifelog/api/v1/events/{id}/full (conversation events, transcriptionContents[].clips[]) and /clsapi/aiTask/getDataAllByTaskId (recordings, including marks, outline, and Mermaid mindMap). `full -date` handles both.",
		"Responses are cached on disk (~/.config/rayneo-cli/cache): past days & reverseGeocode 30d, event/recording details 6h, today/future 5m, volatile queries 10m. Auth & write endpoints are never cached, and task mutations automatically invalidate cached task lists.",
		"Cache flags: -refresh (force network), -no-cache (disable for this run), -cache-ttl 72h (override TTL).",
	},
	Commands: []cmdSpec{
		{Name: "help", Summary: "Print this help. --json emits this exact machine-readable spec.", Args: []argSpec{{Name: "--json", Type: "bool", Desc: "emit JSON spec"}}, Example: "rayneo help --json", ReadOnly: true},
		{Name: "sync", Summary: "[PREFERRED / 自动同步] Configure or pull the active access token from a Scheme A Token Broker (no-refresh; keeps mobile App logged in; mutates local config unless -status — requires user permission).",
			Args: []argSpec{
				{Name: "-url", Type: "url", Desc: "token broker base URL (saved to config.json)"},
				{Name: "-secret", Type: "string", Desc: "token broker Bearer secret (saved to config.json, 0600)"},
				{Name: "-force", Type: "bool", Desc: "skip If-None-Match ETag and force full fetch"},
				{Name: "-verify", Type: "bool", Desc: "verify synced token live via GET /profileapi/v1/profile/query"},
				{Name: "-status", Type: "bool", Desc: "show broker config, masked secret, token fingerprint, and expiry (read-only)"},
				{Name: "-clear", Type: "bool", Desc: "clear broker_url, broker_secret, and broker_etag from config"},
			},
			Example: "rayneo sync -url https://broker.example.com -secret <secret> -verify", Output: "synced token + revision/source/TTL (no_refresh=true)", ReadOnly: false, RequiresConfirmation: true},
		{Name: "har", Summary: "[PREFERRED / 手动抓包] Import access token + device headers from a .har capture without refreshing (mutates local config unless -dry-run — requires user permission).",
			Args:    []argSpec{{Name: "<file.har>", Type: "path", Desc: ".har capture file (or -file / -har)"}, {Name: "-verify", Type: "bool", Desc: "verify token online against /lifelog/api/v1/count"}, {Name: "-dry-run", Type: "bool", Desc: "inspect extracted session without saving (read-only)"}},
			Example: "rayneo har capture.har -verify", Output: "imported token + metadata (no_refresh=true)", ReadOnly: false, RequiresConfirmation: true},
		{Name: "sendcode", Summary: "[Fallback / REQUIRES USER PERMISSION] Ask server to send an SMS login code (prefer `rayneo sync` or `rayneo har <file.har>` so other devices stay logged in).", Method: "POST", Path: "/identify/common/sendVerifyCode",
			Args:    []argSpec{{Name: "-account", Type: "string", Desc: "phone number"}, {Name: "-area", Type: "string", Default: "+86", Desc: "area code with +"}},
			Example: "rayneo sendcode -account 13800000000 -area +86", Output: "{code,message,data}", ReadOnly: false, RequiresConfirmation: true},
		{Name: "login", Summary: "[Fallback / REQUIRES USER PERMISSION] Log in with SMS code (WARNING: invalidates other devices' refreshToken; prefer `rayneo sync` or `rayneo har <file.har>`), or import via -har.", Method: "POST", Path: "/identify/quickLogin",
			Args:    []argSpec{{Name: "-har", Type: "path", Desc: "[preferred] import token from .har instead of SMS login (no-refresh)"}, {Name: "-account", Type: "string", Desc: "phone number"}, {Name: "-code", Type: "string", Desc: "SMS code"}, {Name: "-area", Type: "string", Default: "+86", Desc: "area code"}, {Name: "-country", Type: "string", Default: "cn", Desc: "country code"}},
			Example: "rayneo login -har capture.har", Output: "imported / logged in: accountId=… token=…", ReadOnly: false, RequiresConfirmation: true},
		{Name: "refresh", Summary: "[REQUIRES USER PERMISSION] Manually renew access token (WARNING: immediately invalidates previous access token on mobile App; disabled in broker / HAR / no-refresh mode).", Method: "POST", Path: "/authapi/token/refresh",
			Example: "rayneo refresh", Output: "refreshed: token=… expiresIn=7200s", ReadOnly: false, RequiresConfirmation: true},
		{Name: "whoami", Summary: "Show stored base URL, region, account, token mask, token source, expiry, broker status, refresh policy, deviceId.", ReadOnly: true},
		{Name: "logout", Summary: "[REQUIRES USER PERMISSION] Forget the cached local token and session metadata.", ReadOnly: false, RequiresConfirmation: true},
		{Name: "config", Summary: "[REQUIRES USER PERMISSION] Update local base URL / region / token / HAR session / broker config.",
			Args:    []argSpec{{Name: "-base", Type: "string", Desc: "base URL"}, {Name: "-region", Type: "string", Desc: "cn|us|sg|eu"}, {Name: "-token", Type: "string", Desc: "access token (sets no-refresh)"}, {Name: "-har", Type: "path", Desc: ".har file (sets no-refresh)"}, {Name: "-broker-url", Type: "url", Desc: "token broker base URL"}, {Name: "-broker-secret", Type: "string", Desc: "token broker Bearer secret"}, {Name: "-no-refresh", Type: "bool", Desc: "disable token renewal"}},
			Example: "rayneo config -har capture.har", ReadOnly: false, RequiresConfirmation: true},
		{Name: "count", Summary: "Counts of tasks/schedules/activities.", Method: "GET", Path: "/lifelog/api/v1/count",
			Example: "rayneo count", Output: "{code,data:{countData:[{type,count}]}}", ReadOnly: true},
		{Name: "calendar", Summary: "Per-day lifelog availability + one-line summaries.", Method: "POST", Path: "/lifelog/api/v1/calendar",
			Args:    []argSpec{{Name: "-from", Type: "YYYY-MM", Desc: "start month"}, {Name: "-to", Type: "YYYY-MM", Desc: "end month"}},
			Example: "rayneo calendar -from 2026-01 -to 2026-02", Output: "{data:{dailyLifelogList:[{date,hasLog,dailySummaryList}]}}", ReadOnly: true},
		{Name: "events", Summary: "Events for one day, including trail points.", Method: "GET", Path: "/lifelog/api/v1/events?date=&timezone=",
			Args:    []argSpec{{Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Desc: "the day"}, {Name: "-tz", Type: "string", Default: "Asia/Shanghai", Desc: "timezone"}},
			Example: "rayneo events -date 2026-01-15", Output: "{data:{date,events:[…],trailPoints:[…]}}", ReadOnly: true},
		{Name: "journal", Summary: "Latest AI daily journal (the 'daily story' narrative). Ignores -date.", Method: "GET", Path: "/lifelog/api/v1/journal/latest",
			Example: "rayneo journal", Output: "{data:[{title,periodStart,visualTag,context,…}]}", ReadOnly: true},
		{Name: "jd", Summary: "Journal detail for one specific day (the only way to pick a date).", Method: "POST", Path: "/lifelog/api/v1/journal/detail",
			Args:    []argSpec{{Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Default: "today", Desc: "the day (body key journalDate)"}},
			Example: "rayneo jd -date 2026-01-15", Output: "{data:{context,title,visualTag,countData}}", ReadOnly: true},
		{Name: "trail", Summary: "Export daily GPS trail points & stay points (stays Markdown / GPX / GeoJSON / KML / JSON).", Method: "GET", Path: "/lifelog/api/v1/events",
			Args: []argSpec{
				{Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Default: "today", Desc: "the day"},
				{Name: "-tz", Type: "string", Default: "Asia/Shanghai", Desc: "timezone"},
				{Name: "-format", Type: "stays|gpx|geojson|kml|json", Default: "stays", Desc: "output format"},
				{Name: "-geocode", Type: "bool", Desc: "resolve stay addresses via /profileapi/geo/reverseGeocode"},
				{Name: "-wgs84", Type: "bool", Desc: "convert China GCJ-02 coordinates to WGS-84"},
				{Name: "-out", Type: "file", Desc: "write to file instead of stdout"},
			},
			Example: "rayneo trail -date 2026-11-10 -format gpx -wgs84 -out 2026-11-10.gpx", ReadOnly: true},
		{Name: "digest", Summary: "Obsidian-ready daily note combining story, events timeline, stays, related tasks, and optional transcripts (alias: today).",
			Args: []argSpec{
				{Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Default: "today", Desc: "the day"},
				{Name: "-tz", Type: "string", Default: "Asia/Shanghai", Desc: "timezone"},
				{Name: "-with-transcripts", Type: "bool", Desc: "embed full transcripts in collapsible <details> blocks"},
				{Name: "-geocode", Type: "bool", Desc: "resolve stay locations via reverseGeocode"},
				{Name: "-wgs84", Type: "bool", Desc: "convert stay coordinates to WGS-84"},
				{Name: "-out", Type: "file|dir", Desc: "write Markdown to file or <dir>/<date>.md"},
			},
			Example: "rayneo digest -date yesterday -geocode -out ./obsidian/Daily/", ReadOnly: true},
		{Name: "event", Summary: "One day-event in detail: outline, summary, original sub-events, transcript sources.", Method: "GET", Path: "/lifelog/api/v1/events/{id}",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "event id from `events -date`"}, {Name: "-raw", Type: "bool", Desc: "print raw JSON"}},
			Example: "rayneo event -id 100000000000000001", ReadOnly: true},
		{Name: "full", Summary: "Full event detail incl. per-conversation transcripts (automatically falls back to aiTask recordings).", Method: "GET", Path: "/lifelog/api/v1/events/{id}/full",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "event id"}, {Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Desc: "all events of that day"}, {Name: "-format", Type: "md|txt|srt|json", Default: "md", Desc: "output format"}, {Name: "-out", Type: "dir", Desc: "write files instead of stdout"}},
			Example: "rayneo full -date 2026-01-15 -out ./rayneo-export/transcripts", ReadOnly: true},
		{Name: "recordings", Summary: "List transcription tasks (recordings / live captions) with device filter and pagination.", Method: "POST", Path: "/clsapi/aiTask/v2/selectPage",
			Args: []argSpec{
				{Name: "-type", Type: "string", Default: "recording", Desc: "recording|cc|'' for all"},
				{Name: "-device", Type: "string", Default: "RayNeoS3", Desc: "deviceType filter ('' for all devices)"},
				{Name: "-n", Type: "int", Default: "50", Desc: "page size"},
				{Name: "-deadline", Type: "int64", Desc: "pagination cursor (epoch ms)"},
				{Name: "-all", Type: "bool", Desc: "paginate through all matching recordings"},
				{Name: "-json", Type: "bool", Desc: "print JSON array"},
			},
			Example: "rayneo recordings -type recording -all", ReadOnly: true},
		{Name: "transcript", Summary: "Full transcript (marks, outline, clips, AI summary, Mermaid mindMap) for a recording task.",
			Method: "POST", Path: "/clsapi/aiTask/getDataAllByTaskId",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "taskId"}, {Name: "-date", Type: "YYYY-MM-DD|today|yesterday|-Nd", Desc: "pull all events of that day"}, {Name: "-format", Type: "md|txt|srt|json", Default: "md", Desc: "output format"}, {Name: "-out", Type: "dir", Desc: "write files instead of stdout"}},
			Example: "rayneo transcript -date 2026-01-15 -out ./rayneo-export/transcripts", ReadOnly: true},
		{Name: "suggests", Summary: "AI suggestions list.", Method: "POST", Path: "/summary-service/suggest/list",
			Example: "rayneo suggests", ReadOnly: true},
		{Name: "tasks", Summary: "List or manage tasks (default `list` is read-only; write subcommands `add`/`create`, `done`/`complete`, `reopen`/`undone`, `update`/`edit`, `delete`/`rm` mutate server tasks and REQUIRE EXPLICIT USER PERMISSION).",
			Method: "GET|POST|DELETE", Path: "/action/api/v2/tasks",
			Args: []argSpec{
				{Name: "sub", Type: "list|add|done|reopen|update|delete", Default: "list", Desc: "action (default list is read-only; write subcommands require user permission)"},
				{Name: "-status", Type: "all|pending|completed", Default: "all", Desc: "filter list by status"},
				{Name: "-format", Type: "json|table|md", Default: "json", Desc: "list output format (also -md / -table)"},
				{Name: "-content", Type: "string", Desc: "task title for add/update (or positional text for add)"},
				{Name: "-desc", Type: "string", Desc: "task description for add/update"},
				{Name: "-priority", Type: "normal|high", Default: "normal", Desc: "task priority (normal=1, high=0)"},
				{Name: "-star", Type: "bool", Desc: "starred flag for add/update"},
				{Name: "-due", Type: "YYYY-MM-DD|today|tomorrow", Desc: "due date for add/update"},
				{Name: "-source", Type: "string", Default: "rayneoTasks", Desc: "task source"},
			},
			Example: "rayneo tasks -md | rayneo tasks add \"买牛奶\" -priority high -star | rayneo tasks done <id> | rayneo tasks delete <id>", ReadOnly: false, RequiresConfirmation: true},
		{Name: "chat", Summary: "List AI chat sessions, or pass `-id <chatId>` to fetch full conversation messages.", Method: "GET", Path: "/clsapi/aiChat/selectPage | /clsapi/aiChatMsg/selectPage",
			Args: []argSpec{
				{Name: "-id", Type: "string", Desc: "chatId to fetch full message history"},
				{Name: "-n", Type: "int", Default: "30", Desc: "page size"},
				{Name: "-deadline", Type: "int64", Desc: "pagination cursor (epoch ms)"},
				{Name: "-all", Type: "bool", Desc: "paginate through all messages (with -id)"},
				{Name: "-format", Type: "json|md|table", Desc: "default json for list, md for -id"},
				{Name: "-out", Type: "file", Desc: "write output to file"},
			},
			Example: "rayneo chat -table | rayneo chat -id <chatId> -md", ReadOnly: true},
		{Name: "facts", Summary: "AI memory fact cards (confirmed and proposed).", Method: "GET", Path: "/factsapi/api/v1/facts/list",
			Args:    []argSpec{{Name: "-format", Type: "json|md", Default: "json", Desc: "output format (also -md)"}},
			Example: "rayneo facts -md", ReadOnly: true},
		{Name: "devices", Summary: "Bound RayNeo glasses devices (model, SN, MAC, deviceId).", Method: "GET", Path: "/identify/device/list",
			Args:    []argSpec{{Name: "-format", Type: "json|table|md", Default: "json", Desc: "output format (also -table / -md)"}},
			Example: "rayneo devices -table", ReadOnly: true},
		{Name: "profile", Summary: "Account profile & lifelog settings (pass -onboarding for selfIntro & goals).", Method: "GET", Path: "/profileapi/v1/profile/query",
			Args:    []argSpec{{Name: "-onboarding", Type: "bool", Desc: "also include /lifelog/api/v1/settings/onboarding"}, {Name: "-format", Type: "json|md", Default: "json", Desc: "output format (also -md)"}},
			Example: "rayneo profile -onboarding -md", ReadOnly: true},
		{Name: "quota", Summary: "Membership tier, resource packages (AI models / topics / cloud), and optional consumption history.", Method: "GET", Path: "/balance/members | /balance/usages",
			Args: []argSpec{
				{Name: "-usages", Type: "bool", Desc: "also query /balance/usages"},
				{Name: "-resource-type", Type: "string", Desc: "filter usages: ADVANCED_MODEL|TOPIC|RAYNEO_CLOUD"},
				{Name: "-format", Type: "json|table|md", Default: "json", Desc: "output format (also -table / -md)"},
			},
			Example: "rayneo quota -table", ReadOnly: true},
		{Name: "raw", Summary: "[REQUIRES USER PERMISSION for non-GET / write paths] Arbitrary signed request — use to explore new endpoints.",
			Args:    []argSpec{{Name: "-X", Type: "string", Default: "GET", Desc: "method"}, {Name: "-p", Type: "string", Desc: "path"}, {Name: "-q", Type: "k=v", Desc: "query param (repeatable)"}, {Name: "-d", Type: "json", Desc: "body"}, {Name: "-H", Type: "'K: v'", Desc: "header (repeatable)"}},
			Example: "rayneo raw -X POST -p /lifelog/api/v1/journal/detail -d '{\"journalDate\":\"2026-01-15\"}'", ReadOnly: false, RequiresConfirmation: true},
		{Name: "export", Summary: "Bulk pull a date range to disk (JSON + CSV + Markdown).",
			Args:    []argSpec{{Name: "-from", Type: "YYYY-MM-DD|-Nd", Desc: "start"}, {Name: "-to", Type: "YYYY-MM-DD|today", Desc: "end"}, {Name: "-out", Type: "dir", Default: "rayneo-export", Desc: "output dir"}, {Name: "-format", Type: "json|csv|md|all", Default: "all", Desc: "what to write"}, {Name: "-tz", Type: "string", Default: "Asia/Shanghai", Desc: "timezone"}},
			Example: "rayneo export -from 2026-01-01 -to 2026-01-15 -out ./rayneo-export -format all", Output: "events-<date>.{json,csv} journal-<date>.{json,md}", ReadOnly: true},
		{Name: "cache", Summary: "Inspect/prune/clear the on-disk response cache (stat|ls|prune|clear).",
			Args:    []argSpec{{Name: "sub", Type: "stat|ls|prune|clear", Default: "stat", Desc: "action"}},
			Example: "rayneo cache prune", Output: "cache dir, file count, fresh/stale entries", ReadOnly: true},
		{Name: "report", Summary: "Merge exported journal-*.md into one Markdown report with titles in TOC (offline).",
			Args:    []argSpec{{Name: "-dir", Type: "dir", Default: "rayneo-export", Desc: "export dir"}, {Name: "-out", Type: "file", Default: "report.md", Desc: "output file"}, {Name: "-title", Type: "string", Desc: "report title"}},
			Example: "rayneo report -dir ./rayneo-export -out ./rayneo-export/report.md", Output: "Markdown file", ReadOnly: true},
	},
}

func helpJSON() {
	helpSpec.Version = effectiveVersion()
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(helpSpec)
}
