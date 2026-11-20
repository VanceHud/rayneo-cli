package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func cmdDigest(args []string, debug bool) {
	if code := runDigest(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runDigest(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("digest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dateFlag := fs.String("date", "today", "YYYY-MM-DD, today, yesterday, or -Nd")
	tz := fs.String("tz", "Asia/Shanghai", "timezone")
	withTranscripts := fs.Bool("with-transcripts", false, "include full per-event transcripts in collapsible sections")
	geocode := fs.Bool("geocode", false, "resolve stay locations via /profileapi/geo/reverseGeocode")
	wgs84 := fs.Bool("wgs84", false, "convert China GCJ-02 stay coordinates to WGS-84")
	out := fs.String("out", "", "write Markdown to file or directory (directory writes <date>.md)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	day := resolveDate(*dateFlag)
	cl := newClient(mustConfig(), debug, nil)

	evBody, evEnv, err := fetchDayEventsRaw(cl, day, *tz)
	if err != nil {
		fmt.Fprintln(stderr, "digest events:", err)
		return 1
	}
	rows := eventsFromBody(evBody, day)
	tb := buildTrailBundle(evEnv, day, *tz, *wgs84)
	if *geocode && len(tb.Stays) > 0 {
		enrichStayLocations(cl, tb, evEnv)
	}

	// Fetch daily journal (best-effort: day may not have generated a story yet).
	var jv *journalView
	_, jb, jerr := callWithAuth(cl, "POST", "/lifelog/api/v1/journal/detail", nil,
		map[string]any{"journalDate": day})
	if jerr == nil && !authExpired(jb) {
		jv = journalFromBody(jb)
	}

	// Fetch tasks created or completed on this day (best-effort).
	var dayTasks []taskItem
	if tlist, _, terr := fetchTasksList(cl, defaultTaskSource, nil); terr == nil && tlist != nil {
		for _, it := range tlist.Pending {
			if strings.HasPrefix(it.CreateTime, day) || taskDueMatchesDay(it, day) {
				dayTasks = append(dayTasks, it)
			}
		}
		for _, it := range tlist.Completed {
			if strings.HasPrefix(it.CreateTime, day) ||
				(it.CompletedAt != nil && strings.HasPrefix(*it.CompletedAt, day)) ||
				taskDueMatchesDay(it, day) {
				dayTasks = append(dayTasks, it)
			}
		}
	}

	// Optional full transcripts for each event.
	type eventTranscript struct {
		Title    string
		ID       string
		Clips    int
		Markdown string
	}
	var transcripts []eventTranscript
	if *withTranscripts {
		for i, r := range rows {
			if r.ID == "" {
				continue
			}
			if i > 0 {
				time.Sleep(400 * time.Millisecond)
			}
			if f, ferr := fetchFull(cl, r.ID); ferr == nil {
				transcripts = append(transcripts, eventTranscript{
					Title:    firstNonEmpty(r.Title, f.Title, r.ID),
					ID:       r.ID,
					Clips:    f.allClips(),
					Markdown: f.Markdown(),
				})
				continue
			}
			if b, berr := fetchTranscript(cl, r.ID); berr == nil {
				transcripts = append(transcripts, eventTranscript{
					Title:    firstNonEmpty(r.Title, b.AITask.Name, r.ID),
					ID:       r.ID,
					Clips:    len(b.Recording.Clips),
					Markdown: b.Markdown(),
				})
			}
		}
	}

	// Compute total audio seconds from raw events.
	totalAudioSec := int64(0)
	for _, e := range evEnv.Data.Events {
		sec := toInt64(e["audioLengthSeconds"])
		if sec == 0 {
			sec = toInt64(e["durationSeconds"])
		}
		totalAudioSec += sec
	}

	storyTitle := day + " 全天智记"
	if jv != nil && jv.Title != "" {
		storyTitle = jv.Title
	} else if jv != nil && len(jv.Sections) > 0 && jv.Sections[0].Title != "" {
		storyTitle = jv.Sections[0].Title
	}

	var md strings.Builder
	// YAML Frontmatter (Obsidian / Logseq ready)
	md.WriteString("---\n")
	fmt.Fprintf(&md, "date: %s\n", day)
	fmt.Fprintf(&md, "title: %s\n", strconv.Quote(storyTitle))
	if jv != nil && jv.VisualTag != "" {
		fmt.Fprintf(&md, "mood: %s\n", strconv.Quote(jv.VisualTag))
	}
	if jv != nil && jv.Mode != "" {
		fmt.Fprintf(&md, "mode: %s\n", jv.Mode)
	}
	fmt.Fprintf(&md, "events: %d\n", len(rows))
	fmt.Fprintf(&md, "audio_minutes: %d\n", totalAudioSec/60)
	fmt.Fprintf(&md, "trail_points: %d\n", tb.PointCount)
	fmt.Fprintf(&md, "stays: %d\n", tb.StayCount)
	fmt.Fprintf(&md, "distance_km: %.2f\n", tb.DistanceKm)
	fmt.Fprintf(&md, "tasks_matched: %d\n", len(dayTasks))
	md.WriteString("tags:\n  - rayneo\n  - lifelog\n---\n\n")

	fmt.Fprintf(&md, "# %s · %s\n\n", day, storyTitle)
	meta := []string{
		fmt.Sprintf("**事件** %d", len(rows)),
		fmt.Sprintf("**录音时长** %s", formatDurationHuman(totalAudioSec)),
		fmt.Sprintf("**停留点** %d", tb.StayCount),
		fmt.Sprintf("**里程** %.2f km", tb.DistanceKm),
	}
	if jv != nil && jv.VisualTag != "" {
		meta = append(meta, fmt.Sprintf("**情绪** %s", jv.VisualTag))
	}
	md.WriteString(strings.Join(meta, " · ") + "\n\n")

	// 1. Daily Story
	if jv != nil && (jv.Context != "" || len(jv.Sections) > 0) {
		md.WriteString("## 每日故事\n\n")
		if jv.Context != "" {
			md.WriteString(strings.TrimSpace(jv.Context) + "\n\n")
		}
		for _, sec := range jv.Sections {
			if sec.Title != "" {
				fmt.Fprintf(&md, "### %s\n\n", sec.Title)
			}
			for _, blk := range sec.Blocks {
				md.WriteString(strings.TrimSpace(blk) + "\n\n")
			}
		}
	} else {
		md.WriteString("## 每日故事\n\n_（该日期暂无 AI 每日故事）_\n\n")
	}

	// 2. Events timeline
	fmt.Fprintf(&md, "## 事件时间线（%d）\n\n", len(rows))
	if len(rows) == 0 {
		md.WriteString("_（该日期无事件记录）_\n\n")
	} else {
		md.WriteString("| 时段 | 时长 | 类型 | 标题 | 摘要 |\n")
		md.WriteString("|---|---:|---|---|---|\n")
		for _, r := range rows {
			sum := strings.ReplaceAll(strings.TrimSpace(r.Summary), "\n", " ")
			if len([]rune(sum)) > 80 {
				sum = string([]rune(sum)[:80]) + "…"
			}
			title := strings.ReplaceAll(r.Title, "|", "\\|")
			sum = strings.ReplaceAll(sum, "|", "\\|")
			fmt.Fprintf(&md, "| %s–%s | %s | %s | %s | %s |\n",
				r.Start, r.End, r.Duration, r.Type, title, orDefault(sum, "-"))
		}
		md.WriteString("\n")
	}

	// 3. Stays & Trail
	if tb.StayCount > 0 || tb.PointCount > 0 {
		fmt.Fprintf(&md, "## 行程与停留（%d 处 · %.2f km）\n\n", tb.StayCount, tb.DistanceKm)
		loc := tb.loc()
		if len(tb.Stays) > 0 {
			for _, s := range tb.Stays {
				line := fmt.Sprintf("- **%s–%s**（%s）",
					time.UnixMilli(s.StartTime).In(loc).Format("15:04"),
					time.UnixMilli(s.EndTime).In(loc).Format("15:04"),
					formatDurationHuman(s.DurationS))
				if s.Location != "" {
					line += " · " + s.Location
				}
				if s.EventTitle != "" {
					line += " · " + s.EventTitle
				}
				line += fmt.Sprintf(" (`%.5f, %.5f`)", s.Latitude, s.Longitude)
				md.WriteString(line + "\n")
			}
		} else {
			fmt.Fprintf(&md, "_（记录到 %d 个 GPS 轨迹点，无聚合停留点）_\n", tb.PointCount)
		}
		md.WriteString("\n")
	}

	// 4. Related tasks
	if len(dayTasks) > 0 {
		fmt.Fprintf(&md, "## 当日关联待办（%d）\n\n", len(dayTasks))
		for _, it := range dayTasks {
			box := "[ ]"
			if it.Completed {
				box = "[x]"
			}
			fmt.Fprintf(&md, "- %s %s · `%s`\n", box, it.Content, it.ID)
		}
		md.WriteString("\n")
	}

	// 5. Optional transcripts
	if len(transcripts) > 0 {
		fmt.Fprintf(&md, "## 逐句转录（%d）\n\n", len(transcripts))
		for _, tr := range transcripts {
			fmt.Fprintf(&md, "<details>\n<summary>%s（%d 段 · %s）</summary>\n\n%s\n</details>\n\n",
				tr.Title, tr.Clips, tr.ID, strings.TrimSpace(tr.Markdown))
		}
	}

	content := md.String()
	if *out != "" {
		target := *out
		if info, statErr := os.Stat(target); (statErr == nil && info.IsDir()) || strings.HasSuffix(target, string(os.PathSeparator)) {
			target = filepath.Join(target, day+".md")
		}
		if dir := filepath.Dir(target); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintln(stderr, "digest:", err)
				return 1
			}
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			fmt.Fprintln(stderr, "digest:", err)
			return 1
		}
		fmt.Fprintf(stdout, "digest written: %s (%d events, %d stays, %d tasks)\n",
			target, len(rows), tb.StayCount, len(dayTasks))
		return 0
	}
	fmt.Fprint(stdout, content)
	return 0
}
