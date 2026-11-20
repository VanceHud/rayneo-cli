package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
)

// ---------- /lifelog/api/v1/events/{id}/full ----------

type fullClip struct {
	ClipID        string  `json:"clipId"`
	ClipType      string  `json:"clipType"`
	SpeakerType   string  `json:"speakerType"`
	SpeakerName   string  `json:"speakerName"`
	StartTime     any     `json:"startTime"` // string in /full, int in aiTask
	EndTime       any     `json:"endTime"`
	Content       string  `json:"content"`
	Translate     *string `json:"translateContent"`
	LangCode      string  `json:"languageCode"`
	TranslateLang string  `json:"translateLanguageCode"`
}

type fullGroup struct {
	SourceType string     `json:"sourceType"`
	SourceID   string     `json:"sourceId"`
	Title      string     `json:"title"`
	StartTime  any        `json:"startTime"`
	EndTime    any        `json:"endTime"`
	Clips      []fullClip `json:"clips"`
}

type fullEvent struct {
	ID                   any    `json:"id"`
	Title                string `json:"title"`
	EventType            string `json:"eventType"`
	FlowType             string `json:"flowType"`
	StartTime            any    `json:"startTime"`
	EndTime              any    `json:"endTime"`
	StartTimeFormatted   string `json:"startTimeFormatted"`
	EndTimeFormatted     string `json:"endTimeFormatted"`
	AudioLengthFormatted string `json:"audioLengthFormatted"`
	ParticipantCount     int    `json:"participantCount"`
	SummaryContent       string `json:"summaryContent"`
	Outline              []struct {
		Content   string `json:"content"`
		Timestamp int64  `json:"timestamp"`
	} `json:"outline"`
	TranscriptionContents []fullGroup `json:"transcriptionContents"`
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

// fetchFull pulls the full event detail incl. conversation transcripts.
func fetchFull(cl *api.Client, eventID string) (*fullEvent, error) {
	_, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events/"+eventID+"/full", nil, nil)
	if err != nil {
		return nil, err
	}
	var r struct {
		Code int       `json:"code"`
		Msg  string    `json:"msg"`
		Data fullEvent `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	if r.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", r.Code, r.Msg)
	}
	return &r.Data, nil
}

func (f *fullEvent) allClips() int {
	n := 0
	for _, g := range f.TranscriptionContents {
		n += len(g.Clips)
	}
	return n
}

func (f *fullEvent) baseEpoch() int64 {
	var min int64 = -1
	for _, g := range f.TranscriptionContents {
		if s := toInt64(g.StartTime); s > 0 && (min < 0 || s < min) {
			min = s
		}
	}
	if min < 0 {
		return toInt64(f.StartTime)
	}
	return min
}

func fmtClock(ms int64, base int64) string {
	if ms <= 0 {
		return "--:--"
	}
	rel := ms - base
	if rel < 0 {
		rel = 0
	}
	s := rel / 1000
	return fmt.Sprintf("%02d:%02d:%02d", s/3600, (s%3600)/60, s%60)
}

// Markdown renders the full event with per-conversation transcripts.
func (f *fullEvent) Markdown() string {
	var b strings.Builder
	title := f.Title
	if title == "" {
		title = "全天智记事件"
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	meta := []string{
		fmt.Sprintf("**时间** %s-%s", f.StartTimeFormatted, f.EndTimeFormatted),
		fmt.Sprintf("**时长** %s", f.AudioLengthFormatted),
		fmt.Sprintf("**类型** %s/%s", f.EventType, f.FlowType),
		fmt.Sprintf("**参与者** %d", f.ParticipantCount),
		fmt.Sprintf("**转录片段** %d", f.allClips()),
	}
	b.WriteString(strings.Join(meta, " · ") + "\n\n")

	if f.SummaryContent != "" {
		b.WriteString("## 摘要\n\n" + f.SummaryContent + "\n\n")
	}
	if len(f.Outline) > 0 {
		fmt.Fprintf(&b, "## 要点（%d）\n\n", len(f.Outline))
		for _, o := range f.Outline {
			when := ""
			if o.Timestamp > 0 {
				when = time.UnixMilli(o.Timestamp).Format("15:04") + " "
			}
			fmt.Fprintf(&b, "- %s%s\n", when, o.Content)
		}
		b.WriteString("\n")
	}

	base := f.baseEpoch()
	for i, g := range f.TranscriptionContents {
		fmt.Fprintf(&b, "## 转录 %d/%d · %s\n\n", i+1, len(f.TranscriptionContents), g.Title)
		fmt.Fprintf(&b, "_sourceType: %s · sourceId: `%s` · %d 段_\n\n", g.SourceType, g.SourceID, len(g.Clips))
		for _, c := range g.Clips {
			fmt.Fprintf(&b, "- `%s` **%s**：%s\n", fmtClock(toInt64(c.StartTime), base), c.SpeakerName, c.Content)
			if c.Translate != nil && *c.Translate != "" {
				fmt.Fprintf(&b, "  - _%s_\n", *c.Translate)
			}
		}
		b.WriteString("\n")
	}
	if f.allClips() == 0 {
		b.WriteString("_（该事件没有转录片段）_\n")
	}
	return b.String()
}

// Text renders plain "[hh:mm:ss] speaker: content" lines over all groups.
func (f *fullEvent) Text() string {
	var b strings.Builder
	base := f.baseEpoch()
	for _, g := range f.TranscriptionContents {
		fmt.Fprintf(&b, "=== %s ===\n", g.Title)
		for _, c := range g.Clips {
			b.WriteString(fmt.Sprintf("[%s] %s: %s\n", fmtClock(toInt64(c.StartTime), base), c.SpeakerName, c.Content))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// SRT renders subtitles (times relative to the event start).
func (f *fullEvent) SRT() string {
	var b strings.Builder
	base := f.baseEpoch()
	n := 0
	for _, g := range f.TranscriptionContents {
		for _, c := range g.Clips {
			n++
			text := c.Content
			if c.Translate != nil && *c.Translate != "" {
				text += "\n" + *c.Translate
			}
			b.WriteString(fmt.Sprintf("%d\n%s --> %s\n%s\n\n", n,
				srtTime(toInt64(c.StartTime)-base), srtTime(toInt64(c.EndTime)-base), text))
		}
	}
	return b.String()
}

// writeFullDir writes md/txt/srt/json for one event.
func writeFullDir(dir string, f *fullEvent, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		name + ".md":  f.Markdown(),
		name + ".txt": f.Text(),
		name + ".srt": f.SRT(),
	}
	raw, _ := json.MarshalIndent(f, "", "  ")
	files[name+".json"] = string(raw)
	for fn, content := range files {
		if err := os.WriteFile(filepath.Join(dir, fn), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// speakersOf tallies speakers inside an event.
func (f *fullEvent) speakersOf() string {
	counts := map[string]int{}
	for _, g := range f.TranscriptionContents {
		for _, c := range g.Clips {
			counts[c.SpeakerName]++
		}
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, counts[n]))
	}
	return strings.Join(parts, ", ")
}
