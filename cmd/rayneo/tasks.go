package main

import (
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
)

const (
	taskPriorityHigh   = 0
	taskPriorityNormal = 1
	defaultTaskSource  = "rayneoTasks"
)

type taskItem struct {
	ID               string  `json:"id"`
	Content          string  `json:"content"`
	Description      *string `json:"description"`
	Priority         int     `json:"priority"`
	Starred          bool    `json:"starred"`
	Completed        bool    `json:"completed"`
	CompletedTask    bool    `json:"completedTask"`
	CompletedAt      *string `json:"completedAt"`
	DueDate          any     `json:"dueDate"`
	DueTimeZone      *string `json:"dueTimeZone"`
	Source           string  `json:"source"`
	SourceType       string  `json:"sourceType"`
	SourceID         string  `json:"sourceId"`
	ListID           *string `json:"listId"`
	ClientID         *string `json:"clientId"`
	ClientUpdateTime *string `json:"clientUpdateTime"`
	CreateTime       string  `json:"createTime"`
	UpdateTime       string  `json:"updateTime"`
}

type taskListData struct {
	Pending        []taskItem `json:"pending"`
	Completed      []taskItem `json:"completed"`
	PendingCount   int        `json:"pendingCount"`
	CompletedCount int        `json:"completedCount"`
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Msg     string          `json:"msg"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (e apiEnvelope) errText() string {
	if e.Msg != "" {
		return e.Msg
	}
	return e.Message
}

func parseTaskPriority(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "normal", "default", "1":
		return taskPriorityNormal, nil
	case "high", "urgent", "0":
		return taskPriorityHigh, nil
	default:
		return 0, fmt.Errorf("invalid priority %q (use normal|high or 1|0)", s)
	}
}

func formatTaskPriority(p int) string {
	if p == taskPriorityHigh {
		return "high"
	}
	return "normal"
}

func newTaskClientID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// parseDueDateMs parses human-friendly due date inputs into Unix epoch milliseconds
// required by /action/api/v2/tasks/create and update endpoints.
func parseDueDateMs(s, tzName string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	loc := time.Local
	if tzName != "" {
		if l, err := time.LoadLocation(tzName); err == nil {
			loc = l
		}
	}
	now := time.Now().In(loc)
	endOfDay := func(t time.Time) int64 {
		return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, loc).UnixMilli()
	}

	switch strings.ToLower(s) {
	case "today":
		return endOfDay(now), nil
	case "tomorrow":
		return endOfDay(now.AddDate(0, 0, 1)), nil
	}
	if strings.HasPrefix(s, "+") && strings.HasSuffix(strings.ToLower(s), "d") {
		if n, err := strconv.Atoi(s[1 : len(s)-1]); err == nil && n >= 0 && n <= 3650 {
			return endOfDay(now.AddDate(0, 0, n)), nil
		}
	}
	// Numeric epoch seconds or milliseconds
	if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
		if n < 1_000_000_000_000 {
			return n * 1000, nil
		}
		return n, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), nil
	}
	for _, layout := range []string{
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t.UnixMilli(), nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return endOfDay(t), nil
	}
	return 0, fmt.Errorf("invalid -due %q (use YYYY-MM-DD, YYYY-MM-DD HH:MM, RFC3339, today, tomorrow, +Nd, or epoch ms)", s)
}

func formatTaskDue(v any, tzPtr *string) string {
	if v == nil {
		return ""
	}
	loc := time.Local
	if tzPtr != nil && *tzPtr != "" {
		if l, err := time.LoadLocation(*tzPtr); err == nil {
			loc = l
		}
	}
	if ms := toInt64(v); ms > 0 {
		if ms < 1_000_000_000_000 {
			ms *= 1000
		}
		t := time.UnixMilli(ms).In(loc)
		if t.Hour() == 23 && t.Minute() == 59 {
			return t.Format("2006-01-02")
		}
		return t.Format("2006-01-02 15:04")
	}
	s := strings.TrimSpace(asString(v))
	if s == "" || s == "null" {
		return ""
	}
	return s
}

func taskDueMatchesDay(it taskItem, day string) bool {
	due := formatTaskDue(it.DueDate, it.DueTimeZone)
	return due != "" && strings.HasPrefix(due, day)
}

func fetchTasksList(cl *api.Client, source string, extraQ url.Values) (*taskListData, []byte, error) {
	q := url.Values{}
	for k, vs := range extraQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if cl.Cfg.AccountID != 0 && q.Get("accountId") == "" {
		q.Set("accountId", fmt.Sprintf("%d", cl.Cfg.AccountID))
	}
	if q.Get("sources") == "" {
		if source == "" {
			source = defaultTaskSource
		}
		q.Set("sources", source)
	}
	resp, body, err := callWithAuth(cl, "GET", "/action/api/v2/tasks", q, nil)
	if err != nil {
		return nil, nil, err
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		return nil, body, fmt.Errorf("authentication expired")
	}
	if resp.StatusCode >= 400 {
		return nil, body, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var env struct {
		Code int          `json:"code"`
		Msg  string       `json:"msg"`
		Data taskListData `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, body, err
	}
	if env.Code != 0 {
		return nil, body, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return &env.Data, body, nil
}

func cmdTasks(args []string, debug bool) {
	if code := runTasks(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runTasks(args []string, debug bool, stdout, stderr io.Writer) int {
	sub := "list"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch strings.ToLower(args[0]) {
		case "list", "ls":
			sub = "list"
			rest = args[1:]
		case "add", "create", "new":
			sub = "add"
			rest = args[1:]
		case "done", "complete":
			sub = "done"
			rest = args[1:]
		case "reopen", "undone", "uncomplete":
			sub = "reopen"
			rest = args[1:]
		case "update", "edit":
			sub = "update"
			rest = args[1:]
		case "delete", "rm", "del":
			sub = "delete"
			rest = args[1:]
		case "help", "-h", "--help":
			printTasksUsage(stdout)
			return 0
		default:
			fmt.Fprintf(stderr, "tasks: unknown subcommand %q\n", args[0])
			printTasksUsage(stderr)
			return 2
		}
	}

	switch sub {
	case "list":
		return runTasksList(rest, debug, stdout, stderr)
	case "add":
		return runTasksAdd(rest, debug, stdout, stderr)
	case "done":
		return runTasksAction("complete", rest, debug, stdout, stderr)
	case "reopen":
		return runTasksAction("reopen", rest, debug, stdout, stderr)
	case "update":
		return runTasksUpdate(rest, debug, stdout, stderr)
	case "delete":
		return runTasksDelete(rest, debug, stdout, stderr)
	default:
		printTasksUsage(stderr)
		return 2
	}
}

func printTasksUsage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  [read-only]
  rayneo tasks [list] [-status all|pending|completed] [-format json|table|md] [-source rayneoTasks]

  [NON-READ-ONLY · REQUIRE EXPLICIT USER PERMISSION]
  rayneo tasks add <content> [-desc text] [-priority normal|high] [-star] [-due YYYY-MM-DD|today|tomorrow|+Nd] [-tz Asia/Shanghai] [-source rayneoTasks] [-list-id id] [-json]
  rayneo tasks done <id...> [-source rayneoTasks] [-list-id id] [-json]
  rayneo tasks reopen <id...> [-source rayneoTasks] [-list-id id] [-json]
  rayneo tasks update <id> [-content text] [-desc text] [-priority normal|high] [-star=true|false] [-completed=true|false] [-due YYYY-MM-DD] [-tz Asia/Shanghai] [-json]
  rayneo tasks delete <id...> [-source rayneoTasks] [-list-id id] [-json]`)
}

func runTasksList(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	status := fs.String("status", "all", "all|pending|completed")
	format := fs.String("format", "json", "json|table|md")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	tableFlag := fs.Bool("table", false, "shorthand for -format table")
	source := fs.String("source", defaultTaskSource, "task source (rayneoTasks|googleTasks|microsoftTodo|systemReminders)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *mdFlag {
		fmtMode = "md"
	} else if *tableFlag {
		fmtMode = "table"
	}
	stFilter := strings.ToLower(strings.TrimSpace(*status))
	if stFilter != "all" && stFilter != "pending" && stFilter != "completed" {
		fmt.Fprintf(stderr, "tasks: invalid -status %q (use all|pending|completed)\n", *status)
		return 2
	}

	c := mustConfig()
	cl := newClient(c, debug, headers(hs))
	data, rawBody, err := fetchTasksList(cl, *source, params(kv))
	if err != nil {
		if len(rawBody) > 0 && fmtMode == "json" {
			printJSONTo(stdout, rawBody)
		}
		fmt.Fprintln(stderr, "tasks:", err)
		return 1
	}

	if fmtMode == "json" && stFilter == "all" {
		printJSONTo(stdout, rawBody)
		return 0
	}

	filtered := *data
	switch stFilter {
	case "pending":
		filtered.Completed = nil
		filtered.CompletedCount = 0
	case "completed":
		filtered.Pending = nil
		filtered.PendingCount = 0
	}

	switch fmtMode {
	case "json":
		b, _ := json.MarshalIndent(filtered, "", "  ")
		fmt.Fprintln(stdout, string(b))
	case "md":
		fmt.Fprint(stdout, renderTasksMarkdown(&filtered, stFilter))
	case "table":
		fmt.Fprint(stdout, renderTasksTable(&filtered, stFilter))
	default:
		fmt.Fprintf(stderr, "tasks: invalid -format %q (use json|table|md)\n", *format)
		return 2
	}
	return 0
}

func renderTasksMarkdown(d *taskListData, filter string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 待办事项（待完成 %d · 已完成 %d）\n\n", len(d.Pending), len(d.Completed))
	writeGroup := func(title string, items []taskItem, checked bool) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "## %s（%d）\n\n", title, len(items))
		box := "[ ]"
		if checked {
			box = "[x]"
		}
		for _, it := range items {
			var tags []string
			if it.Priority == taskPriorityHigh {
				tags = append(tags, "高优")
			}
			if it.Starred {
				tags = append(tags, "★")
			}
			if due := formatTaskDue(it.DueDate, it.DueTimeZone); due != "" {
				tags = append(tags, "截止 "+due)
			}
			meta := ""
			if len(tags) > 0 {
				meta = " _(" + strings.Join(tags, " · ") + ")_"
			}
			fmt.Fprintf(&b, "- %s %s%s · `%s`\n", box, it.Content, meta, it.ID)
			if it.Description != nil && strings.TrimSpace(*it.Description) != "" {
				fmt.Fprintf(&b, "  - %s\n", strings.TrimSpace(*it.Description))
			}
		}
		b.WriteString("\n")
	}
	if filter != "completed" {
		writeGroup("待完成", d.Pending, false)
	}
	if filter != "pending" {
		writeGroup("已完成", d.Completed, true)
	}
	if len(d.Pending) == 0 && len(d.Completed) == 0 {
		b.WriteString("_（暂无待办事项）_\n")
	}
	return b.String()
}

func renderTasksTable(d *taskListData, filter string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-20s %-6s %-6s %-4s %-12s %s\n", "ID", "STATE", "PRIO", "STAR", "DUE", "CONTENT")
	writeRows := func(items []taskItem, state string) {
		for _, it := range items {
			star := "-"
			if it.Starred {
				star = "*"
			}
			due := "-"
			if d := formatTaskDue(it.DueDate, it.DueTimeZone); d != "" {
				due = d
				if len(due) > 10 {
					due = due[:10]
				}
			}
			fmt.Fprintf(&b, "%-20s %-6s %-6s %-4s %-12s %s\n",
				it.ID, state, formatTaskPriority(it.Priority), star, due, it.Content)
		}
	}
	if filter != "completed" {
		writeRows(d.Pending, "todo")
	}
	if filter != "pending" {
		writeRows(d.Completed, "done")
	}
	fmt.Fprintf(&b, "\n共 %d 条待完成 / %d 条已完成\n", len(d.Pending), len(d.Completed))
	return b.String()
}

func runTasksAdd(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
	fs.SetOutput(stderr)
	content := fs.String("content", "", "task title/content")
	desc := fs.String("desc", "", "task description")
	priority := fs.String("priority", "normal", "normal|high (or 1|0)")
	star := fs.Bool("star", false, "mark task as starred")
	starred := fs.Bool("starred", false, "alias for -star")
	completed := fs.Bool("completed", false, "create task as already completed")
	due := fs.String("due", "", "due date (YYYY-MM-DD, today, tomorrow, or ISO-8601)")
	tz := fs.String("tz", "Asia/Shanghai", "timezone for dueDate")
	source := fs.String("source", defaultTaskSource, "task source")
	listID := fs.String("list-id", "", "optional task list ID")
	jsonOut := fs.Bool("json", false, "print raw JSON response")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")

	valueFlags := map[string]bool{
		"content": true, "desc": true, "priority": true, "due": true,
		"tz": true, "source": true, "list-id": true, "H": true,
	}
	if err := fs.Parse(reorderPositionalLast(args, valueFlags)); err != nil {
		return 2
	}

	title := strings.TrimSpace(*content)
	if title == "" && fs.NArg() > 0 {
		title = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if title == "" {
		fmt.Fprintln(stderr, "tasks add: task content is required, e.g. `rayneo tasks add \"买牛奶\"`")
		return 2
	}
	prioVal, err := parseTaskPriority(*priority)
	if err != nil {
		fmt.Fprintln(stderr, "tasks add:", err)
		return 2
	}
	src := strings.TrimSpace(*source)
	if src == "" {
		src = defaultTaskSource
	}

	payload := map[string]any{
		"content":   title,
		"priority":  prioVal,
		"starred":   *star || *starred,
		"completed": *completed,
		"source":    src,
		"clientId":  newTaskClientID(),
	}
	if d := strings.TrimSpace(*desc); d != "" {
		payload["description"] = d
	}
	if strings.TrimSpace(*due) != "" {
		dueMs, err := parseDueDateMs(*due, *tz)
		if err != nil {
			fmt.Fprintln(stderr, "tasks add:", err)
			return 2
		}
		payload["dueDate"] = dueMs
		if strings.TrimSpace(*tz) != "" {
			payload["dueTimeZone"] = strings.TrimSpace(*tz)
		}
	}
	if lid := strings.TrimSpace(*listID); lid != "" {
		payload["listId"] = lid
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	resp, body, err := callWithAuth(cl, "POST", "/action/api/v2/tasks/create", nil, payload)
	if err != nil {
		fmt.Fprintln(stderr, "tasks add:", err)
		return 1
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		if *jsonOut {
			printJSONTo(stdout, body)
		}
		return 1
	}
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil || resp.StatusCode >= 400 || env.Code != 0 {
		printJSONTo(stdout, body)
		fmt.Fprintf(stderr, "tasks add failed: HTTP %d code=%d msg=%s\n", resp.StatusCode, env.Code, env.errText())
		return 1
	}
	if cl.Cache != nil {
		cl.Cache.InvalidatePrefix("/action/api/v2/tasks")
	}
	if *jsonOut {
		printJSONTo(stdout, body)
		return 0
	}
	var created struct {
		ID     string `json:"id"`
		Source string `json:"source"`
	}
	_ = json.Unmarshal(env.Data, &created)
	fmt.Fprintf(stdout, "created task: id=%s priority=%s starred=%v content=%q\n",
		orDefault(created.ID, "(pending)"), formatTaskPriority(prioVal), *star || *starred, title)
	return 0
}

func runTasksAction(action string, args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tasks "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	idFlag := fs.String("id", "", "task ID")
	source := fs.String("source", defaultTaskSource, "task source")
	listID := fs.String("list-id", "", "optional task list ID")
	jsonOut := fs.Bool("json", false, "print raw JSON response")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")

	valueFlags := map[string]bool{"id": true, "source": true, "list-id": true, "H": true}
	if err := fs.Parse(reorderPositionalLast(args, valueFlags)); err != nil {
		return 2
	}
	ids := collectIDs(*idFlag, fs.Args())
	if len(ids) == 0 {
		fmt.Fprintf(stderr, "tasks %s: at least one task ID is required\n", action)
		return 2
	}
	src := strings.TrimSpace(*source)
	if src == "" {
		src = defaultTaskSource
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	failed := 0
	for i, tid := range ids {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		payload := map[string]any{
			"source":           src,
			"clientUpdateTime": strconv.FormatInt(time.Now().UnixMilli(), 10),
		}
		if lid := strings.TrimSpace(*listID); lid != "" {
			payload["listId"] = lid
		}
		path := "/action/api/v2/tasks/" + url.PathEscape(tid) + "/" + action
		resp, body, err := callWithAuth(cl, "POST", path, nil, payload)
		if err != nil {
			fmt.Fprintf(stderr, "tasks %s %s: %v\n", action, tid, err)
			failed++
			continue
		}
		if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
			if *jsonOut {
				printJSONTo(stdout, body)
			}
			return 1
		}
		var env apiEnvelope
		if err := json.Unmarshal(body, &env); err != nil || resp.StatusCode >= 400 || env.Code != 0 {
			fmt.Fprintf(stderr, "tasks %s %s failed: HTTP %d code=%d msg=%s\n", action, tid, resp.StatusCode, env.Code, env.errText())
			if *jsonOut {
				printJSONTo(stdout, body)
			}
			failed++
			continue
		}
		if *jsonOut {
			printJSONTo(stdout, body)
		} else {
			verb := "completed"
			if action == "reopen" {
				verb = "reopened"
			}
			fmt.Fprintf(stdout, "%s task: id=%s\n", verb, tid)
		}
	}
	if cl.Cache != nil {
		cl.Cache.InvalidatePrefix("/action/api/v2/tasks")
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func runTasksUpdate(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tasks update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idFlag := fs.String("id", "", "task ID")
	content := fs.String("content", "", "new task content")
	desc := fs.String("desc", "", "new description (pass empty string with -desc='' to clear)")
	priority := fs.String("priority", "", "normal|high (or 1|0)")
	star := fs.Bool("star", false, "set starred state (use -star=true or -star=false)")
	starred := fs.Bool("starred", false, "alias for -star")
	completed := fs.Bool("completed", false, "set completed state (use -completed=true or -completed=false)")
	due := fs.String("due", "", "due date (YYYY-MM-DD, today, tomorrow, or '' to clear)")
	tz := fs.String("tz", "", "timezone for dueDate")
	source := fs.String("source", defaultTaskSource, "task source")
	listID := fs.String("list-id", "", "optional task list ID")
	jsonOut := fs.Bool("json", false, "print raw JSON response")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")

	valueFlags := map[string]bool{
		"id": true, "content": true, "desc": true, "priority": true,
		"due": true, "tz": true, "source": true, "list-id": true, "H": true,
	}
	if err := fs.Parse(reorderPositionalLast(args, valueFlags)); err != nil {
		return 2
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	tid := strings.TrimSpace(*idFlag)
	if tid == "" && fs.NArg() > 0 {
		tid = strings.TrimSpace(fs.Arg(0))
	}
	if tid == "" {
		fmt.Fprintln(stderr, "tasks update: task ID is required, e.g. `rayneo tasks update <id> -content \"新内容\"`")
		return 2
	}
	if !setFlags["content"] && !setFlags["desc"] && !setFlags["priority"] &&
		!setFlags["star"] && !setFlags["starred"] && !setFlags["completed"] &&
		!setFlags["due"] && !setFlags["tz"] {
		fmt.Fprintln(stderr, "tasks update: specify at least one field to update (-content, -desc, -priority, -star, -completed, -due)")
		return 2
	}

	var prioVal int
	if setFlags["priority"] {
		var err error
		prioVal, err = parseTaskPriority(*priority)
		if err != nil {
			fmt.Fprintln(stderr, "tasks update:", err)
			return 2
		}
	}

	src := strings.TrimSpace(*source)
	if src == "" {
		src = defaultTaskSource
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	// Fetch existing task first so omitted fields are preserved rather than wiped by a full-object update.
	var existing *taskItem
	oldForce := false
	if cl.Cache != nil {
		oldForce = cl.Cache.Force
		cl.Cache.Force = true
	}
	if list, _, err := fetchTasksList(cl, src, nil); err == nil && list != nil {
		for i := range list.Pending {
			if list.Pending[i].ID == tid {
				existing = &list.Pending[i]
				break
			}
		}
		if existing == nil {
			for i := range list.Completed {
				if list.Completed[i].ID == tid {
					existing = &list.Completed[i]
					break
				}
			}
		}
	}
	if cl.Cache != nil {
		cl.Cache.Force = oldForce
	}

	payload := map[string]any{
		"source":           src,
		"clientUpdateTime": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	if existing != nil {
		payload["content"] = existing.Content
		payload["priority"] = existing.Priority
		payload["starred"] = existing.Starred
		payload["completed"] = existing.Completed
		if existing.Description != nil {
			payload["description"] = *existing.Description
		}
		if existing.DueDate != nil {
			if ms := toInt64(existing.DueDate); ms > 0 {
				payload["dueDate"] = ms
			} else {
				payload["dueDate"] = existing.DueDate
			}
		}
		if existing.DueTimeZone != nil {
			payload["dueTimeZone"] = *existing.DueTimeZone
		}
		if existing.ListID != nil && *existing.ListID != "" {
			payload["listId"] = *existing.ListID
		}
		if existing.ClientID != nil && *existing.ClientID != "" {
			payload["clientId"] = *existing.ClientID
		}
		if existing.Source != "" && !setFlags["source"] {
			payload["source"] = existing.Source
		}
	}

	if setFlags["content"] {
		payload["content"] = strings.TrimSpace(*content)
	}
	if setFlags["desc"] {
		if strings.TrimSpace(*desc) == "" {
			payload["description"] = nil
		} else {
			payload["description"] = strings.TrimSpace(*desc)
		}
	}
	if setFlags["priority"] {
		payload["priority"] = prioVal
	}
	if setFlags["star"] {
		payload["starred"] = *star
	}
	if setFlags["starred"] {
		payload["starred"] = *starred
	}
	if setFlags["completed"] {
		payload["completed"] = *completed
	}
	if setFlags["due"] {
		if strings.TrimSpace(*due) == "" {
			payload["dueDate"] = nil
		} else {
			tzEff := strings.TrimSpace(*tz)
			if tzEff == "" {
				if existingTZ, ok := payload["dueTimeZone"].(string); ok && existingTZ != "" {
					tzEff = existingTZ
				} else {
					tzEff = "Asia/Shanghai"
				}
			}
			dueMs, err := parseDueDateMs(*due, tzEff)
			if err != nil {
				fmt.Fprintln(stderr, "tasks update:", err)
				return 2
			}
			payload["dueDate"] = dueMs
			if !setFlags["tz"] && payload["dueTimeZone"] == nil {
				payload["dueTimeZone"] = tzEff
			}
		}
	}
	if setFlags["tz"] {
		if strings.TrimSpace(*tz) == "" {
			payload["dueTimeZone"] = nil
		} else {
			payload["dueTimeZone"] = strings.TrimSpace(*tz)
		}
	}
	if lid := strings.TrimSpace(*listID); lid != "" {
		payload["listId"] = lid
	}

	path := "/action/api/v2/tasks/" + url.PathEscape(tid)
	resp, body, err := callWithAuth(cl, "POST", path, nil, payload)
	if err != nil {
		fmt.Fprintln(stderr, "tasks update:", err)
		return 1
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		if *jsonOut {
			printJSONTo(stdout, body)
		}
		return 1
	}
	var env apiEnvelope
	if err := json.Unmarshal(body, &env); err != nil || resp.StatusCode >= 400 || env.Code != 0 {
		printJSONTo(stdout, body)
		fmt.Fprintf(stderr, "tasks update failed: HTTP %d code=%d msg=%s\n", resp.StatusCode, env.Code, env.errText())
		return 1
	}
	if cl.Cache != nil {
		cl.Cache.InvalidatePrefix("/action/api/v2/tasks")
	}
	if *jsonOut {
		printJSONTo(stdout, body)
		return 0
	}
	fmt.Fprintf(stdout, "updated task: id=%s content=%q priority=%v starred=%v completed=%v\n",
		tid, payload["content"], payload["priority"], payload["starred"], payload["completed"])
	return 0
}

func runTasksDelete(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tasks delete", flag.ContinueOnError)
	fs.SetOutput(stderr)
	idFlag := fs.String("id", "", "task ID")
	source := fs.String("source", defaultTaskSource, "task source")
	listID := fs.String("list-id", "", "optional task list ID")
	jsonOut := fs.Bool("json", false, "print raw JSON response")
	var hs kvFlag
	fs.Var(&hs, "H", "extra header")

	valueFlags := map[string]bool{"id": true, "source": true, "list-id": true, "H": true}
	if err := fs.Parse(reorderPositionalLast(args, valueFlags)); err != nil {
		return 2
	}
	ids := collectIDs(*idFlag, fs.Args())
	if len(ids) == 0 {
		fmt.Fprintln(stderr, "tasks delete: at least one task ID is required")
		return 2
	}
	src := strings.TrimSpace(*source)
	if src == "" {
		src = defaultTaskSource
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	failed := 0
	for i, tid := range ids {
		if i > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		q := url.Values{}
		q.Set("source", src)
		if lid := strings.TrimSpace(*listID); lid != "" {
			q.Set("listId", lid)
		}
		path := "/action/api/v2/tasks/" + url.PathEscape(tid)
		resp, body, err := callWithAuth(cl, "DELETE", path, q, nil)
		if err != nil {
			fmt.Fprintf(stderr, "tasks delete %s: %v\n", tid, err)
			failed++
			continue
		}
		if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
			if *jsonOut {
				printJSONTo(stdout, body)
			}
			return 1
		}
		var env apiEnvelope
		if err := json.Unmarshal(body, &env); err != nil || resp.StatusCode >= 400 || env.Code != 0 {
			fmt.Fprintf(stderr, "tasks delete %s failed: HTTP %d code=%d msg=%s\n", tid, resp.StatusCode, env.Code, env.errText())
			if *jsonOut {
				printJSONTo(stdout, body)
			}
			failed++
			continue
		}
		if *jsonOut {
			printJSONTo(stdout, body)
		} else {
			fmt.Fprintf(stdout, "deleted task: id=%s\n", tid)
		}
	}
	if cl.Cache != nil {
		cl.Cache.InvalidatePrefix("/action/api/v2/tasks")
	}
	if failed > 0 {
		return 1
	}
	return 0
}

func collectIDs(flagID string, pos []string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			p := strings.TrimSpace(part)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if flagID != "" {
		add(flagID)
	}
	for _, a := range pos {
		add(a)
	}
	return out
}
