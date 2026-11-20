package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
)

// ---------- models ----------

type clip struct {
	ClipID              string  `json:"clipId"`
	ClipType            string  `json:"clipType"`
	Content             string  `json:"content"`
	SpeakerID           string  `json:"speakerId"`
	SpeakerName         string  `json:"speakerName"`
	SpeakerType         string  `json:"speakerType"`
	LanguageCode        string  `json:"languageCode"`
	TranslateContent    *string `json:"translateContent"`
	TranslateLanguageCd string  `json:"translateLanguageCode"`
	StartTime           int64   `json:"startTime"`
	EndTime             int64   `json:"endTime"`
}

type recording struct {
	TaskID           string          `json:"taskId"`
	RecordName       string          `json:"recordName"`
	SessionName      string          `json:"sessionName"`
	AudioDuration    int64           `json:"audioDuration"`
	AudioDurationFmt string          `json:"audioDurationFormatted"`
	SourceLanguage   string          `json:"sourceLanguageCode"`
	TargetLanguage   string          `json:"targetLanguageCode"`
	Clips            []clip          `json:"clips"`
	Marks            json.RawMessage `json:"marks"`
	Outline          json.RawMessage `json:"outline"`
	SpeakerSeparated bool            `json:"separateSpeakers"`
	TextLength       int             `json:"textLength"`
}

type aiTaskRow struct {
	TaskID     string `json:"taskId"`
	TaskType   string `json:"taskType"`
	Name       string `json:"name"`
	Status     int    `json:"status"`
	DurationMs int64  `json:"recordingDurationMs"`
	CreateTime int64  `json:"createTime"`
	DeviceType string `json:"deviceType"`
}

type transcriptBundle struct {
	AITask    aiTaskRow       `json:"aiTask"`
	Recording recording       `json:"recording"`
	Summary   json.RawMessage `json:"summary"`
}

// ---------- fetch ----------

// listAITasks returns one page of transcription tasks (taskType recording/cc/…).
// deviceType defaults to "RayNeoS3" when passed as non-empty; empty string sends null (all devices).
// deadline <= 0 uses 9999999999999 (latest page).
func listAITasks(cl *api.Client, taskType, deviceType string, pageSize int, deadline int64) ([]aiTaskRow, error) {
	if deadline <= 0 {
		deadline = 9999999999999
	}
	payload := map[string]any{
		"deviceType":    nil,
		"taskIds":       nil,
		"phoneDeviceId": nil,
		"taskType":      nil,
		"name":          nil,
		"deadline":      deadline,
		"pageSize":      pageSize,
	}
	if deviceType != "" {
		payload["deviceType"] = deviceType
	}
	if taskType != "" {
		payload["taskType"] = taskType
	}
	_, body, err := callWithAuth(cl, "POST", "/clsapi/aiTask/v2/selectPage", nil, payload)
	if err != nil {
		return nil, err
	}
	if authExpired(body) {
		return nil, fmt.Errorf("authentication expired")
	}
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			PageList []aiTaskRow `json:"pageList"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", r.Code, r.Msg)
	}
	return r.Data.PageList, nil
}

// listAITasksAll paginates through /clsapi/aiTask/v2/selectPage using createTime as deadline cursor.
func listAITasksAll(cl *api.Client, taskType, deviceType string, pageSize int, startDeadline int64) ([]aiTaskRow, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	cursor := startDeadline
	seen := map[string]bool{}
	var all []aiTaskRow
	for page := 0; page < 50; page++ {
		if page > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		rows, err := listAITasks(cl, taskType, deviceType, pageSize, cursor)
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
		for _, row := range rows {
			if row.CreateTime > 0 && (minTS == 0 || row.CreateTime < minTS) {
				minTS = row.CreateTime
			}
			if seen[row.TaskID] {
				continue
			}
			seen[row.TaskID] = true
			all = append(all, row)
			added++
		}
		if len(rows) < pageSize || added == 0 || minTS <= 1 {
			break
		}
		cursor = minTS - 1
	}
	return all, nil
}

// fetchTranscript pulls the full transcript for one taskId.
func fetchTranscript(cl *api.Client, taskID string) (*transcriptBundle, error) {
	_, body, err := callWithAuth(cl, "POST", "/clsapi/aiTask/getDataAllByTaskId", nil,
		map[string]any{"taskIds": []string{taskID}})
	if err != nil {
		return nil, err
	}
	var r struct {
		Code int                `json:"code"`
		Msg  string             `json:"msg"`
		Data []transcriptBundle `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 || len(r.Data) == 0 {
		return nil, fmt.Errorf("task %s: code=%d msg=%s", taskID, r.Code, r.Msg)
	}
	b := r.Data[0]
	return &b, nil
}

// ---------- rendering ----------

func (b *transcriptBundle) t0() int64 {
	var min int64 = -1
	for _, c := range b.Recording.Clips {
		if min < 0 || c.StartTime < min {
			min = c.StartTime
		}
	}
	if min < 0 {
		return b.AITask.CreateTime
	}
	return min
}

func mmss(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	s := ms / 1000
	return fmt.Sprintf("%02d:%02d", s/60, s%60)
}

func srtTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3600000
	ms %= 3600000
	m := ms / 60000
	ms %= 60000
	s := ms / 1000
	ms %= 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// Text renders "[mm:ss] speaker: content" lines (translation appended if present).
func (b *transcriptBundle) Text() string {
	var sb strings.Builder
	t0 := b.t0()
	for _, c := range b.Recording.Clips {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s", mmss(c.StartTime-t0), c.SpeakerName, c.Content))
		if c.TranslateContent != nil && *c.TranslateContent != "" {
			sb.WriteString("\n         ↳ " + *c.TranslateContent)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// SRT renders a subtitle file.
func (b *transcriptBundle) SRT() string {
	var sb strings.Builder
	t0 := b.t0()
	for i, c := range b.Recording.Clips {
		text := c.Content
		if c.TranslateContent != nil && *c.TranslateContent != "" {
			text += "\n" + *c.TranslateContent
		}
		sb.WriteString(fmt.Sprintf("%d\n%s --> %s\n%s\n\n", i+1,
			srtTime(c.StartTime-t0), srtTime(c.EndTime-t0), text))
	}
	return sb.String()
}

type recordingMark struct {
	MarkTime any    `json:"markTime"`
	MarkID   string `json:"markId"`
	Remarks  []struct {
		Hashtag *string `json:"hashtag"`
		Remark  string  `json:"remark"`
	} `json:"remarks"`
}

type recordingOutlineItem struct {
	Offset    any    `json:"offset"`
	Title     string `json:"title"`
	Timestamp any    `json:"timestamp"`
	Subtitle  string `json:"subtitle"`
}

type mindMapNode struct {
	Title    string        `json:"title"`
	Children []mindMapNode `json:"children"`
}

type summaryItem struct {
	Name    string `json:"name"`
	Model   string `json:"model"`
	Content struct {
		Content string       `json:"content"`
		MindMap *mindMapNode `json:"mindMap"`
	} `json:"content"`
}

func sanitizeMindMapLabel(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer(
		"\r\n", " ",
		"\n", " ",
		"\r", " ",
		"(", "（",
		")", "）",
		"[", "【",
		"]", "】",
		"{", "｛",
		"}", "｝",
		"\"", "'",
	)
	return replacer.Replace(s)
}

func writeMindMapMermaid(sb *strings.Builder, node *mindMapNode, depth int) {
	if node == nil {
		return
	}
	label := sanitizeMindMapLabel(node.Title)
	if label == "" {
		label = "未命名节点"
	}
	indent := strings.Repeat("  ", depth+1)
	if depth == 0 {
		fmt.Fprintf(sb, "%sroot((%s))\n", indent, label)
	} else {
		fmt.Fprintf(sb, "%s%s\n", indent, label)
	}
	for i := range node.Children {
		writeMindMapMermaid(sb, &node.Children[i], depth+1)
	}
}

// Markdown renders a readable transcript with metadata, marks, outline, speakers, clips, summaries and mind maps.
func (b *transcriptBundle) Markdown() string {
	var sb strings.Builder
	name := b.AITask.Name
	if name == "" {
		name = b.Recording.RecordName
	}
	fmt.Fprintf(&sb, "# %s\n\n", name)
	when := time.UnixMilli(b.AITask.CreateTime).Format("2006-01-02 15:04")
	meta := []string{
		fmt.Sprintf("**时间** %s", when),
		fmt.Sprintf("**时长** %s", b.Recording.AudioDurationFmt),
		fmt.Sprintf("**语言** %s", b.Recording.SourceLanguage),
		fmt.Sprintf("**片段** %d 段", len(b.Recording.Clips)),
		fmt.Sprintf("**taskId** `%s`", b.AITask.TaskID),
	}
	if b.Recording.TargetLanguage != "" {
		meta = append(meta, fmt.Sprintf("**译文** %s", b.Recording.TargetLanguage))
	}
	sb.WriteString(strings.Join(meta, " · ") + "\n\n")

	// speaker tally
	counts := map[string]int{}
	for _, c := range b.Recording.Clips {
		counts[c.SpeakerName]++
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	var parts []string
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, counts[n]))
	}
	if len(parts) > 0 {
		fmt.Fprintf(&sb, "**说话人** %s\n\n", strings.Join(parts, ", "))
	}

	t0 := b.t0()

	// recording marks (glasses highlight markers)
	if len(b.Recording.Marks) > 0 && string(b.Recording.Marks) != "null" {
		var marks []recordingMark
		if json.Unmarshal(b.Recording.Marks, &marks) == nil && len(marks) > 0 {
			fmt.Fprintf(&sb, "## 重点打点（%d）\n\n", len(marks))
			for _, m := range marks {
				mt := toInt64(m.MarkTime)
				tsLabel := mmss(mt - t0)
				if mt > 0 {
					tsLabel = fmt.Sprintf("%s (%s)", mmss(mt-t0), time.UnixMilli(mt).Format("15:04:05"))
				}
				var notes []string
				for _, rm := range m.Remarks {
					var chunk []string
					if rm.Hashtag != nil && strings.TrimSpace(*rm.Hashtag) != "" {
						chunk = append(chunk, "#"+strings.TrimSpace(*rm.Hashtag))
					}
					if strings.TrimSpace(rm.Remark) != "" {
						chunk = append(chunk, strings.TrimSpace(rm.Remark))
					}
					if len(chunk) > 0 {
						notes = append(notes, strings.Join(chunk, " "))
					}
				}
				if len(notes) > 0 {
					fmt.Fprintf(&sb, "- `%s` %s\n", tsLabel, strings.Join(notes, "；"))
				} else {
					fmt.Fprintf(&sb, "- `%s` _（眼镜打点标记）_\n", tsLabel)
				}
			}
			sb.WriteString("\n")
		}
	}

	// recording outline
	if len(b.Recording.Outline) > 0 && string(b.Recording.Outline) != "null" {
		var outline []recordingOutlineItem
		if json.Unmarshal(b.Recording.Outline, &outline) == nil && len(outline) > 0 {
			fmt.Fprintf(&sb, "## 提纲（%d）\n\n", len(outline))
			for _, o := range outline {
				ts := toInt64(o.Timestamp)
				if ts == 0 {
					ts = toInt64(o.Offset)
				}
				prefix := ""
				if ts > 0 {
					if ts > 1_000_000_000_000 { // epoch ms
						prefix = fmt.Sprintf("`%s` ", mmss(ts-t0))
					} else {
						prefix = fmt.Sprintf("`%s` ", mmss(ts))
					}
				}
				text := strings.TrimSpace(o.Subtitle)
				if strings.TrimSpace(o.Title) != "" {
					if text != "" {
						text = fmt.Sprintf("**%s**：%s", strings.TrimSpace(o.Title), text)
					} else {
						text = fmt.Sprintf("**%s**", strings.TrimSpace(o.Title))
					}
				}
				if text != "" {
					fmt.Fprintf(&sb, "- %s%s\n", prefix, text)
				}
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString("## 转录\n\n")
	for _, c := range b.Recording.Clips {
		fmt.Fprintf(&sb, "- `%s` **%s**：%s\n", mmss(c.StartTime-t0), c.SpeakerName, c.Content)
		if c.TranslateContent != nil && *c.TranslateContent != "" {
			fmt.Fprintf(&sb, "  - _%s_\n", *c.TranslateContent)
		}
	}

	// summaries & mind maps
	if len(b.Summary) > 0 && string(b.Summary) != "null" {
		var arr []summaryItem
		if json.Unmarshal(b.Summary, &arr) == nil && len(arr) > 0 {
			for i, s := range arr {
				if strings.TrimSpace(s.Content.Content) != "" {
					heading := "AI 摘要"
					if len(arr) > 1 {
						heading = fmt.Sprintf("AI 摘要 %d", i+1)
						if strings.TrimSpace(s.Name) != "" {
							heading += " · " + strings.TrimSpace(s.Name)
						}
					}
					fmt.Fprintf(&sb, "\n## %s\n\n", heading)
					if s.Model != "" {
						fmt.Fprintf(&sb, "_模型：%s_\n\n", s.Model)
					}
					sb.WriteString(strings.TrimSpace(s.Content.Content))
					sb.WriteString("\n")
				}
				if s.Content.MindMap != nil && (strings.TrimSpace(s.Content.MindMap.Title) != "" || len(s.Content.MindMap.Children) > 0) {
					sb.WriteString("\n## 思维导图\n\n```mermaid\nmindmap\n")
					writeMindMapMermaid(&sb, s.Content.MindMap, 0)
					sb.WriteString("```\n")
				}
			}
		}
	}
	return sb.String()
}

// writeTranscriptDir writes all supported formats into dir.
func writeTranscriptDir(dir string, b *transcriptBundle, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		name + ".md":  b.Markdown(),
		name + ".txt": b.Text(),
		name + ".srt": b.SRT(),
	}
	raw, _ := json.MarshalIndent(b, "", "  ")
	files[name+".json"] = string(raw)
	for fn, content := range files {
		if err := os.WriteFile(filepath.Join(dir, fn), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
