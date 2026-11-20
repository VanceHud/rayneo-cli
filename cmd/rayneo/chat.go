package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
)

type chatSessionRow struct {
	ID         string `json:"id"`
	ChatID     string `json:"chatId"`
	Name       string `json:"name"`
	ChatSource string `json:"chatSource"`
	CreateTime int64  `json:"createTime"`
	UpdateTime int64  `json:"updateTime"`
	TimeZone   string `json:"timeZone"`
}

type chatMsgRow struct {
	ID          string          `json:"id"`
	ChatID      string          `json:"chatId"`
	MsgID       string          `json:"msgId"`
	ParentMsgID string          `json:"parentMsgId"`
	Role        string          `json:"role"`
	Model       string          `json:"model"`
	MsgSource   string          `json:"msgSource"`
	MsgType     int             `json:"msgType"`
	CreateTime  int64           `json:"createTime"`
	Content     json.RawMessage `json:"content"`
}

var thoughtTagRe = regexp.MustCompile(`(?s)<thought>(.*?)</thought>`)

func extractChatMsgText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asStr string
	if json.Unmarshal(raw, &asStr) == nil {
		return strings.TrimSpace(asStr)
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return strings.TrimSpace(string(raw))
	}
	for _, k := range []string{"text", "respondQuery", "askModelText", "content", "message"} {
		if v, ok := obj[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	b, _ := json.MarshalIndent(obj, "", "  ")
	return string(b)
}

func fetchChatMessagesPage(cl *api.Client, chatID string, pageSize int, deadline int64, extraQ url.Values) ([]chatMsgRow, []byte, error) {
	q := url.Values{}
	for k, vs := range extraQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	q.Set("chatId", chatID)
	if pageSize > 0 {
		q.Set("pageSize", strconv.Itoa(pageSize))
	}
	if deadline > 0 {
		q.Set("deadline", strconv.FormatInt(deadline, 10))
	}
	resp, body, err := callWithAuth(cl, "GET", "/clsapi/aiChatMsg/selectPage", q, nil)
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
		Data []chatMsgRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, body, err
	}
	if env.Code != 0 {
		return nil, body, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, body, nil
}

func fetchChatMessagesAll(cl *api.Client, chatID string, pageSize int, startDeadline int64, extraQ url.Values) ([]chatMsgRow, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	cursor := startDeadline
	seen := map[string]bool{}
	var all []chatMsgRow
	for page := 0; page < 50; page++ {
		if page > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		rows, _, err := fetchChatMessagesPage(cl, chatID, pageSize, cursor, extraQ)
		if err != nil {
			if len(all) > 0 {
				return all, err
			}
			return nil, err
		}
		if len(rows) == 0 {
			break
		}
		added := 0
		var minTS int64
		for _, m := range rows {
			if m.CreateTime > 0 && (minTS == 0 || m.CreateTime < minTS) {
				minTS = m.CreateTime
			}
			key := firstNonEmpty(m.MsgID, m.ID, fmt.Sprintf("%d-%s", m.CreateTime, m.Role))
			if seen[key] {
				continue
			}
			seen[key] = true
			all = append(all, m)
			added++
		}
		if len(rows) < pageSize || added == 0 || minTS <= 1 {
			break
		}
		cursor = minTS - 1
	}
	return all, nil
}

func renderChatMessagesMarkdown(chatID string, msgs []chatMsgRow) string {
	sorted := make([]chatMsgRow, len(msgs))
	copy(sorted, msgs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreateTime != sorted[j].CreateTime {
			return sorted[i].CreateTime < sorted[j].CreateTime
		}
		// Keep user before assistant if timestamps tie.
		if sorted[i].Role != sorted[j].Role {
			return sorted[i].Role == "user"
		}
		return i < j
	})

	var b strings.Builder
	fmt.Fprintf(&b, "# AI 对话记录 `%s`\n\n", chatID)
	fmt.Fprintf(&b, "**消息数** %d\n\n", len(sorted))
	if len(sorted) == 0 {
		b.WriteString("_（该会话暂无消息）_\n")
		return b.String()
	}
	for _, m := range sorted {
		roleLabel := m.Role
		switch strings.ToLower(m.Role) {
		case "user":
			roleLabel = "用户"
		case "assistant":
			roleLabel = "助手"
		}
		var meta []string
		if m.CreateTime > 0 {
			meta = append(meta, time.UnixMilli(m.CreateTime).Format("2006-01-02 15:04:05"))
		}
		if m.Model != "" {
			meta = append(meta, m.Model)
		}
		if m.MsgSource != "" {
			meta = append(meta, m.MsgSource)
		}
		header := fmt.Sprintf("## %s", roleLabel)
		if len(meta) > 0 {
			header += " · " + strings.Join(meta, " · ")
		}
		b.WriteString(header + "\n\n")

		text := extractChatMsgText(m.Content)
		if text == "" {
			b.WriteString("_（空消息）_\n\n")
			continue
		}
		// Convert <thought>...</thought> blocks into collapsible <details>.
		thoughts := thoughtTagRe.FindAllStringSubmatch(text, -1)
		cleaned := strings.TrimSpace(thoughtTagRe.ReplaceAllString(text, ""))
		for _, th := range thoughts {
			body := strings.TrimSpace(th[1])
			if body != "" {
				fmt.Fprintf(&b, "<details>\n<summary>思考过程</summary>\n\n%s\n\n</details>\n\n", body)
			}
		}
		if cleaned != "" {
			b.WriteString(cleaned + "\n\n")
		}
	}
	return b.String()
}

func cmdChat(args []string, debug bool) {
	if code := runChat(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runChat(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	id := fs.String("id", "", "chatId: fetch full messages of this AI chat session")
	n := fs.Int("n", 30, "page size")
	deadline := fs.Int64("deadline", 0, "optional pagination cursor (epoch ms)")
	all := fs.Bool("all", false, "paginate to fetch all messages when -id is set")
	format := fs.String("format", "", "json|md|table (default: json for list, md for -id)")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	tableFlag := fs.Bool("table", false, "shorthand for -format table")
	jsonFlag := fs.Bool("json", false, "shorthand for -format json")
	out := fs.String("out", "", "write output to file path instead of stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *mdFlag {
		fmtMode = "md"
	} else if *tableFlag {
		fmtMode = "table"
	} else if *jsonFlag {
		fmtMode = "json"
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	extraQ := params(kv)

	// Single chat session detail mode: /clsapi/aiChatMsg/selectPage
	if strings.TrimSpace(*id) != "" {
		if fmtMode == "" {
			fmtMode = "md"
		}
		chatID := strings.TrimSpace(*id)
		var msgs []chatMsgRow
		var rawBody []byte
		var err error
		if *all {
			msgs, err = fetchChatMessagesAll(cl, chatID, *n, *deadline, extraQ)
		} else {
			msgs, rawBody, err = fetchChatMessagesPage(cl, chatID, *n, *deadline, extraQ)
		}
		if err != nil {
			if len(rawBody) > 0 && fmtMode == "json" {
				printJSONTo(stdout, rawBody)
			}
			fmt.Fprintln(stderr, "chat:", err)
			return 1
		}
		var rendered string
		switch fmtMode {
		case "json":
			if !*all && len(rawBody) > 0 {
				var v any
				if json.Unmarshal(rawBody, &v) == nil {
					b, _ := json.MarshalIndent(v, "", "  ")
					rendered = string(b) + "\n"
				} else {
					rendered = string(rawBody) + "\n"
				}
			} else {
				b, _ := json.MarshalIndent(map[string]any{"chatId": chatID, "count": len(msgs), "messages": msgs}, "", "  ")
				rendered = string(b) + "\n"
			}
		case "md":
			rendered = renderChatMessagesMarkdown(chatID, msgs)
		default:
			fmt.Fprintf(stderr, "chat -id: invalid -format %q (use md|json)\n", fmtMode)
			return 2
		}
		return writeOrPrint(rendered, *out, stdout, stderr, "chat")
	}

	// Session list mode: /clsapi/aiChat/selectPage
	if fmtMode == "" {
		fmtMode = "json"
	}
	q := url.Values{}
	for k, vs := range extraQ {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	if q.Get("pageSize") == "" {
		q.Set("pageSize", strconv.Itoa(*n))
	}
	if *deadline > 0 && q.Get("deadline") == "" {
		q.Set("deadline", strconv.FormatInt(*deadline, 10))
	}
	resp, body, err := callWithAuth(cl, "GET", "/clsapi/aiChat/selectPage", q, nil)
	if err != nil {
		fmt.Fprintln(stderr, "chat:", err)
		return 1
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		if fmtMode == "json" {
			printJSONTo(stdout, body)
		}
		return 1
	}
	if fmtMode == "json" {
		if *out != "" {
			var v any
			if json.Unmarshal(body, &v) == nil {
				b, _ := json.MarshalIndent(v, "", "  ")
				return writeOrPrint(string(b)+"\n", *out, stdout, stderr, "chat")
			}
			return writeOrPrint(string(body)+"\n", *out, stdout, stderr, "chat")
		}
		printJSONTo(stdout, body)
		return 0
	}

	var env struct {
		Code int              `json:"code"`
		Msg  string           `json:"msg"`
		Data []chatSessionRow `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code != 0 {
		fmt.Fprintf(stderr, "chat: decode failed (code=%d msg=%s)\n", env.Code, env.Msg)
		return 1
	}
	var sb strings.Builder
	switch fmtMode {
	case "table":
		fmt.Fprintf(&sb, "%-34s %-16s %-8s %s\n", "CHATID", "UPDATED", "SOURCE", "TITLE")
		for _, s := range env.Data {
			ts := s.UpdateTime
			if ts == 0 {
				ts = s.CreateTime
			}
			when := ""
			if ts > 0 {
				when = time.UnixMilli(ts).Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&sb, "%-34s %-16s %-8s %s\n", s.ChatID, when, orDefault(s.ChatSource, "-"), s.Name)
		}
		fmt.Fprintf(&sb, "\n共 %d 条会话\n", len(env.Data))
	case "md":
		fmt.Fprintf(&sb, "# AI 对话会话列表（%d）\n\n", len(env.Data))
		for _, s := range env.Data {
			ts := s.UpdateTime
			if ts == 0 {
				ts = s.CreateTime
			}
			when := ""
			if ts > 0 {
				when = time.UnixMilli(ts).Format("2006-01-02 15:04")
			}
			fmt.Fprintf(&sb, "- **%s** · %s · `%s` · `%s`\n", s.Name, when, orDefault(s.ChatSource, "-"), s.ChatID)
		}
	default:
		fmt.Fprintf(stderr, "chat: invalid -format %q (use json|table|md)\n", fmtMode)
		return 2
	}
	return writeOrPrint(sb.String(), *out, stdout, stderr, "chat")
}

func writeOrPrint(content, outPath string, stdout, stderr io.Writer, label string) int {
	if outPath == "" {
		fmt.Fprint(stdout, content)
		return 0
	}
	if dir := filepath.Dir(outPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", label, err)
			return 1
		}
	}
	if err := os.WriteFile(outPath, []byte(content), 0o644); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", label, err)
		return 1
	}
	fmt.Fprintf(stdout, "%s written: %s\n", label, outPath)
	return 0
}
