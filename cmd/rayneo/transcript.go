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

// listAITasks returns transcription tasks (taskType recording/cc/…).
func listAITasks(cl *api.Client, taskType string, pageSize int) ([]aiTaskRow, error) {
	payload := map[string]any{
		"deviceType":    "RayNeoS3",
		"taskIds":       nil,
		"phoneDeviceId": nil,
		"taskType":      nil,
		"name":          nil,
		"deadline":      9999999999999,
		"pageSize":      pageSize,
	}
	if taskType != "" {
		payload["taskType"] = taskType
	}
	_, body, err := cl.Call("POST", "/clsapi/aiTask/v2/selectPage", nil, payload)
	if err != nil {
		return nil, err
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

// fetchTranscript pulls the full transcript for one taskId.
func fetchTranscript(cl *api.Client, taskID string) (*transcriptBundle, error) {
	_, body, err := cl.Call("POST", "/clsapi/aiTask/getDataAllByTaskId", nil,
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

// Markdown renders a readable transcript with metadata and speakers.
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

	sb.WriteString("## 转录\n\n")
	t0 := b.t0()
	for _, c := range b.Recording.Clips {
		fmt.Fprintf(&sb, "- `%s` **%s**：%s\n", mmss(c.StartTime-t0), c.SpeakerName, c.Content)
		if c.TranslateContent != nil && *c.TranslateContent != "" {
			fmt.Fprintf(&sb, "  - _%s_\n", *c.TranslateContent)
		}
	}
	if len(b.Summary) > 0 && string(b.Summary) != "null" {
		var arr []struct {
			Content struct {
				Content string `json:"content"`
			} `json:"content"`
		}
		if json.Unmarshal(b.Summary, &arr) == nil && len(arr) > 0 && arr[0].Content.Content != "" {
			sb.WriteString("\n## AI 摘要\n\n")
			sb.WriteString(arr[0].Content.Content)
			sb.WriteString("\n")
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
