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

	b.WriteString("## 目录\n\n")
	for _, f := range jfiles {
		base := strings.TrimSuffix(filepath.Base(f), ".md")
		date := strings.TrimPrefix(base, "journal-")
		fmt.Fprintf(&b, "- [%s](#%s)\n", date, strings.ReplaceAll(date, ".", ""))
	}
	b.WriteString("\n---\n\n")

	for _, f := range jfiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		body := string(data)
		lines := strings.SplitN(body, "\n", 2)
		head := lines[0]
		base := strings.TrimSuffix(filepath.Base(f), ".md")
		date := strings.TrimPrefix(base, "journal-")
		// demote headings by one level and re-anchor on the date
		fmt.Fprintf(&b, "<a id=\"%s\"></a>\n\n", strings.ReplaceAll(date, ".", ""))
		fmt.Fprintf(&b, "## %s\n\n", date)
		if len(lines) > 1 {
			rest := lines[1]
			_ = head
			b.WriteString(strings.TrimSpace(rest))
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
	Name     string    `json:"name"`
	Summary  string    `json:"summary"`
	Method   string    `json:"http_method,omitempty"`
	Path     string    `json:"http_path,omitempty"`
	Args     []argSpec `json:"args,omitempty"`
	Example  string    `json:"example,omitempty"`
	Output   string    `json:"output,omitempty"`
	ReadOnly bool      `json:"read_only"`
}

var helpSpec = struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Description string    `json:"description"`
	Auth        string    `json:"auth"`
	Notes       []string  `json:"notes"`
	Commands    []cmdSpec `json:"commands"`
}{
	Name:        "rayneo",
	Version:     "dev",
	Description: "Query RayNeo AI lifelog (全天智记) data: events, calendar, AI daily journal, tasks, chats, suggestions. Prefer HAR token mode (`rayneo har <file.har>` / `-har <file.har>`) so mobile/other devices stay logged in; every request is signed with TCL1-HMAC-SHA256.",
	Auth:        "[PREFERRED / 优先使用] `rayneo har <capture.har> -verify` (or one-off `-har <capture.har>` / `RAYNEO_HAR=<capture.har>`): extracts `xtcl-access-token` from packet capture with refresh disabled, avoiding kicking mobile/other devices offline.\n      [FALLBACK  / 备选慎用] `sendcode -account <phone>` -> `login -account <phone> -code <code>`: SMS login invalidates other devices' refreshToken, and `/authapi/token/refresh` immediately invalidates the active access token.",
	Notes: []string{
		"AUTH PRIORITY: Always prefer `rayneo har <file.har>` (or `-har <file.har>`) over `sendcode`/`login`/`refresh`. Re-logging in via SMS invalidates the phone App's refreshToken, and refreshing invalidates its current access token.",
		"All query commands are read-only. No write/delete endpoints are implemented.",
		"HAR mode (`rayneo har <file.har>` or global `-har <file.har>` / `RAYNEO_HAR`) extracts `xtcl-access-token` and safe client headers while clearing `refreshToken` and setting `no_refresh=true`.",
		"-no-refresh disables token renewal even if a refreshToken is present.",
		"-version prints `rayneo <version>` and exits.",
		"-debug prints the canonical string, stringToSign and Authorization header for each request.",
		"-H 'Name: value' adds/overrides a raw header; -q k=v adds a query param (both repeatable).",
		"Optional env overrides: RAYNEO_HAR, RAYNEO_TOKEN, RAYNEO_NO_REFRESH, RAYNEO_APP_ID, RAYNEO_APP_KEY.",
		"JSON is printed pretty to stdout; exit code 1 on transport/auth failure.",
		"Rate: keep it gentle; use export with its built-in 400ms spacing for bulk pulls.",
		"Transcripts live in two stores: /lifelog/api/v1/events/{id}/full (conversation events, transcriptionContents[].clips[]) and /clsapi/aiTask/getDataAllByTaskId (recordings). `full -date` handles both.",
		"Responses are cached on disk (~/.config/rayneo-cli/cache): past days 30d, today 5m, volatile queries 10m. Auth endpoints are never cached.",
		"Cache flags: -refresh (force network), -no-cache (disable for this run), -cache-ttl 72h (override TTL).",
	},
	Commands: []cmdSpec{
		{Name: "help", Summary: "Print this help. --json emits this exact machine-readable spec.", Args: []argSpec{{Name: "--json", Type: "bool", Desc: "emit JSON spec"}}, Example: "rayneo help --json", ReadOnly: true},
		{Name: "har", Summary: "[PREFERRED / 优先推荐] Import access token + device headers from a .har capture without refreshing (avoids logging out other devices).",
			Args:    []argSpec{{Name: "<file.har>", Type: "path", Desc: ".har capture file (or -file / -har)"}, {Name: "-verify", Type: "bool", Desc: "verify token online against /lifelog/api/v1/count"}, {Name: "-dry-run", Type: "bool", Desc: "inspect extracted session without saving"}},
			Example: "rayneo har capture.har -verify", Output: "imported token + metadata (no_refresh=true)", ReadOnly: false},
		{Name: "sendcode", Summary: "[Fallback] Ask server to send an SMS login code (prefer `rayneo har <file.har>` so other devices stay logged in).", Method: "POST", Path: "/identify/common/sendVerifyCode",
			Args:    []argSpec{{Name: "-account", Type: "string", Desc: "phone number"}, {Name: "-area", Type: "string", Default: "+86", Desc: "area code with +"}},
			Example: "rayneo sendcode -account 13800000000 -area +86", Output: "{code,message,data}", ReadOnly: false},
		{Name: "login", Summary: "[Fallback] Log in with SMS code (WARNING: invalidates other devices' refreshToken; prefer `rayneo har <file.har>`), or import via -har.", Method: "POST", Path: "/identify/quickLogin",
			Args:    []argSpec{{Name: "-har", Type: "path", Desc: "[preferred] import token from .har instead of SMS login (no-refresh)"}, {Name: "-account", Type: "string", Desc: "phone number"}, {Name: "-code", Type: "string", Desc: "SMS code"}, {Name: "-area", Type: "string", Default: "+86", Desc: "area code"}, {Name: "-country", Type: "string", Default: "cn", Desc: "country code"}},
			Example: "rayneo login -har capture.har", Output: "imported / logged in: accountId=… token=…", ReadOnly: false},
		{Name: "refresh", Summary: "Manually renew access token (WARNING: invalidates previous access token; disabled in HAR / no-refresh mode).", Method: "POST", Path: "/authapi/token/refresh",
			Example: "rayneo refresh", Output: "refreshed: token=… expiresIn=7200s", ReadOnly: false},
		{Name: "whoami", Summary: "Show stored base URL, region, account, token mask, token source, refresh policy, deviceId.", ReadOnly: true},
		{Name: "logout", Summary: "Forget the cached token.", ReadOnly: false},
		{Name: "config", Summary: "Update base URL / region / token / HAR session.",
			Args:    []argSpec{{Name: "-base", Type: "string", Desc: "base URL"}, {Name: "-region", Type: "string", Desc: "cn|us|sg|eu"}, {Name: "-token", Type: "string", Desc: "access token (sets no-refresh)"}, {Name: "-har", Type: "path", Desc: ".har file (sets no-refresh)"}, {Name: "-no-refresh", Type: "bool", Desc: "disable token renewal"}},
			Example: "rayneo config -har capture.har", ReadOnly: false},
		{Name: "count", Summary: "Counts of tasks/schedules/activities.", Method: "GET", Path: "/lifelog/api/v1/count",
			Example: "rayneo count", Output: "{code,data:{countData:[{type,count}]}}", ReadOnly: true},
		{Name: "calendar", Summary: "Per-day lifelog availability + one-line summaries.", Method: "POST", Path: "/lifelog/api/v1/calendar",
			Args:    []argSpec{{Name: "-from", Type: "YYYY-MM", Desc: "start month"}, {Name: "-to", Type: "YYYY-MM", Desc: "end month"}},
			Example: "rayneo calendar -from 2026-01 -to 2026-02", Output: "{data:{dailyLifelogList:[{date,hasLog,dailySummaryList}]}}", ReadOnly: true},
		{Name: "events", Summary: "Events for one day, including trail points.", Method: "GET", Path: "/lifelog/api/v1/events?date=&timezone=",
			Args:    []argSpec{{Name: "-date", Type: "YYYY-MM-DD", Desc: "the day"}, {Name: "-tz", Type: "string", Default: "Asia/Shanghai", Desc: "timezone"}},
			Example: "rayneo events -date 2026-01-15", Output: "{data:{date,events:[…],trailPoints:[…]}}", ReadOnly: true},
		{Name: "journal", Summary: "Latest AI daily journal (the 'daily story' narrative). Ignores -date.", Method: "GET", Path: "/lifelog/api/v1/journal/latest",
			Example: "rayneo journal", Output: "{data:[{title,periodStart,visualTag,context,…}]}", ReadOnly: true},
		{Name: "jd", Summary: "Journal detail for one specific day (the only way to pick a date).", Method: "POST", Path: "/lifelog/api/v1/journal/detail",
			Args:    []argSpec{{Name: "-date", Type: "YYYY-MM-DD", Desc: "the day (body key journalDate)"}},
			Example: "rayneo jd -date 2026-01-15", Output: "{data:{context,title,visualTag,countData}}", ReadOnly: true},
		{Name: "event", Summary: "One day-event in detail: outline, summary, original sub-events, transcript sources.", Method: "GET", Path: "/lifelog/api/v1/events/{id}",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "event id from `events -date`"}, {Name: "-raw", Type: "bool", Desc: "print raw JSON"}},
			Example: "rayneo event -id 100000000000000001", ReadOnly: true},
		{Name: "full", Summary: "Full event detail incl. per-conversation transcripts (the transcript the app shows).", Method: "GET", Path: "/lifelog/api/v1/events/{id}/full",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "event id"}, {Name: "-date", Type: "YYYY-MM-DD", Desc: "all events of that day"}, {Name: "-format", Type: "md|txt|srt|json", Default: "md", Desc: "output format"}, {Name: "-out", Type: "dir", Desc: "write files instead of stdout"}},
			Example: "rayneo full -date 2026-01-15 -out ./rayneo-export/transcripts", ReadOnly: true},
		{Name: "recordings", Summary: "List transcription tasks (recordings / live captions).", Method: "POST", Path: "/clsapi/aiTask/v2/selectPage",
			Args:    []argSpec{{Name: "-type", Type: "string", Default: "recording", Desc: "recording|cc|'' for all"}, {Name: "-n", Type: "int", Default: "50", Desc: "page size"}},
			Example: "rayneo recordings -type recording", ReadOnly: true},
		{Name: "transcript", Summary: "Full transcript (per-sentence clips with speaker + timestamps) for a recording task.",
			Method: "POST", Path: "/clsapi/aiTask/getDataAllByTaskId",
			Args:    []argSpec{{Name: "-id", Type: "string", Desc: "taskId"}, {Name: "-date", Type: "YYYY-MM-DD", Desc: "pull all events of that day"}, {Name: "-format", Type: "md|txt|srt|json", Default: "md", Desc: "output format"}, {Name: "-out", Type: "dir", Desc: "write files instead of stdout"}},
			Example: "rayneo transcript -date 2026-01-15 -out ./rayneo-export/transcripts", ReadOnly: true},
		{Name: "suggests", Summary: "AI suggestions list.", Method: "POST", Path: "/summary-service/suggest/list",
			Example: "rayneo suggests", ReadOnly: true},
		{Name: "tasks", Summary: "Action items created by the assistant.", Method: "GET", Path: "/action/api/v2/tasks?accountId=&sources=rayneoTasks",
			Example: "rayneo tasks", ReadOnly: true},
		{Name: "chat", Summary: "AI chat history page.", Method: "GET", Path: "/clsapi/aiChat/selectPage?pageSize=30",
			Example: "rayneo chat", ReadOnly: true},
		{Name: "raw", Summary: "Arbitrary signed request — use to explore new endpoints.",
			Args:    []argSpec{{Name: "-X", Type: "string", Default: "GET", Desc: "method"}, {Name: "-p", Type: "string", Desc: "path"}, {Name: "-q", Type: "k=v", Desc: "query param (repeatable)"}, {Name: "-d", Type: "json", Desc: "body"}, {Name: "-H", Type: "'K: v'", Desc: "header (repeatable)"}},
			Example: "rayneo raw -X POST -p /lifelog/api/v1/journal/detail -d '{\"journalDate\":\"2026-01-15\"}'", ReadOnly: false},
		{Name: "export", Summary: "Bulk pull a date range to disk (JSON + CSV + Markdown).",
			Args:    []argSpec{{Name: "-from", Type: "YYYY-MM-DD", Desc: "start"}, {Name: "-to", Type: "YYYY-MM-DD", Desc: "end"}, {Name: "-out", Type: "dir", Default: "rayneo-export", Desc: "output dir"}, {Name: "-format", Type: "json|csv|md|all", Default: "all", Desc: "what to write"}},
			Example: "rayneo export -from 2026-01-01 -to 2026-01-15 -out ./rayneo-export -format all", Output: "events-<date>.{json,csv} journal-<date>.{json,md}", ReadOnly: true},
		{Name: "cache", Summary: "Inspect/clear the on-disk response cache (stat|ls|clear).",
			Args:    []argSpec{{Name: "sub", Type: "stat|ls|clear", Default: "stat", Desc: "action"}},
			Example: "rayneo cache stat", Output: "cache dir, file count, fresh/stale entries", ReadOnly: true},
		{Name: "report", Summary: "Merge exported journal-*.md into one Markdown report (offline).",
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
