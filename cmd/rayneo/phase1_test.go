package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/config"
)

func setupMockGateway(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("RAYNEO_BROKER_URL", "")
	t.Setenv("RAYNEO_BROKER_SECRET", "")
	t.Setenv("RAYNEO_HAR", "")
	t.Setenv("RAYNEO_TOKEN", "")
	t.Setenv("RAYNEO_NO_REFRESH", "")

	oldHar := harPath
	oldNoRef := noRefreshFlag
	oldBURL := brokerURLFlag
	oldBSec := brokerSecretFlag
	oldCacheEnabled := cacheEnabled
	oldCacheForce := cacheForce
	harPath = ""
	noRefreshFlag = false
	brokerURLFlag = ""
	brokerSecretFlag = ""
	cacheEnabled = true
	cacheForce = false
	t.Cleanup(func() {
		harPath = oldHar
		noRefreshFlag = oldNoRef
		brokerURLFlag = oldBURL
		brokerSecretFlag = oldBSec
		cacheEnabled = oldCacheEnabled
		cacheForce = oldCacheForce
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		BaseURL:     srv.URL,
		Region:      "cn",
		Token:       "mock_access_token_1234567890",
		NoRefresh:   true,
		TokenSource: "test",
		AccountID:   100000000000001,
		DeviceID:    "test-device-id",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save mock config: %v", err)
	}
	return srv
}

func TestTasksFullLifecycleCRUD(t *testing.T) {
	var mu sync.Mutex
	tasks := map[string]taskItem{
		"101": {
			ID: "101", Content: "旧任务", Priority: taskPriorityNormal,
			Starred: false, Completed: false, Source: "rayneoTasks",
			CreateTime: "2026-11-20T09:00:00",
		},
	}
	listHits := 0
	var lastCreateBody map[string]any
	var lastUpdateBody map[string]any
	var lastDeleteQuery string
	var lastCompletePath, lastReopenPath string

	setupMockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		bodyBytes, _ := io.ReadAll(r.Body)

		switch {
		case r.Method == "GET" && r.URL.Path == "/action/api/v2/tasks":
			listHits++
			var pending, completed []taskItem
			for _, it := range tasks {
				if it.Completed {
					completed = append(completed, it)
				} else {
					pending = append(pending, it)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"pending": pending, "completed": completed,
					"pendingCount": len(pending), "completedCount": len(completed),
				},
			})

		case r.Method == "POST" && r.URL.Path == "/action/api/v2/tasks/create":
			_ = json.Unmarshal(bodyBytes, &lastCreateBody)
			prio := taskPriorityNormal
			if p, ok := lastCreateBody["priority"].(float64); ok {
				prio = int(p)
			}
			starred, _ := lastCreateBody["starred"].(bool)
			item := taskItem{
				ID: "202", Content: asString(lastCreateBody["content"]),
				Priority: prio, Starred: starred, Completed: false, Source: "rayneoTasks",
				CreateTime: "2026-11-20T10:00:00",
			}
			tasks["202"] = item
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{"id": "202", "source": "rayneoTasks"},
			})

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/complete"):
			lastCompletePath = r.URL.Path
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/action/api/v2/tasks/"), "/complete")
			if it, ok := tasks[id]; ok {
				it.Completed = true
				tasks[id] = it
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": nil})

		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/reopen"):
			lastReopenPath = r.URL.Path
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/action/api/v2/tasks/"), "/reopen")
			if it, ok := tasks[id]; ok {
				it.Completed = false
				tasks[id] = it
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": nil})

		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/action/api/v2/tasks/"):
			id := strings.TrimPrefix(r.URL.Path, "/action/api/v2/tasks/")
			_ = json.Unmarshal(bodyBytes, &lastUpdateBody)
			if it, ok := tasks[id]; ok {
				if c, ok := lastUpdateBody["content"].(string); ok {
					it.Content = c
				}
				if s, ok := lastUpdateBody["starred"].(bool); ok {
					it.Starred = s
				}
				if p, ok := lastUpdateBody["priority"].(float64); ok {
					it.Priority = int(p)
				}
				tasks[id] = it
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": tasks[id]})

		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/action/api/v2/tasks/"):
			lastDeleteQuery = r.URL.RawQuery
			id := strings.TrimPrefix(r.URL.Path, "/action/api/v2/tasks/")
			delete(tasks, id)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "success", "data": nil})

		default:
			http.NotFound(w, r)
		}
	})

	// 1) Initial list warms cache
	var out, errBuf bytes.Buffer
	if code := runTasks([]string{"-md"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks list failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "旧任务") {
		t.Fatalf("expected existing task in md output, got: %s", out.String())
	}

	// 2) Add task (high priority => 0, starred => true) and verify cache invalidation
	out.Reset()
	errBuf.Reset()
	if code := runTasks([]string{"add", "买牛奶", "-desc", "全脂鲜奶", "-priority", "high", "-star", "-due", "2026-11-25"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks add failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "id=202") {
		t.Fatalf("expected created id=202, got: %s", out.String())
	}
	if lastCreateBody["priority"] != float64(taskPriorityHigh) || lastCreateBody["starred"] != true || lastCreateBody["source"] != "rayneoTasks" {
		t.Fatalf("unexpected create payload: %#v", lastCreateBody)
	}
	if dueMs, ok := lastCreateBody["dueDate"].(float64); !ok || dueMs < 1_000_000_000_000 {
		t.Fatalf("expected dueDate as epoch milliseconds, got: %#v", lastCreateBody["dueDate"])
	}

	// List again should hit server (cache was invalidated) and include new task 202.
	out.Reset()
	if code := runTasks([]string{"-table"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks table failed (%d): %s", code, errBuf.String())
	}
	if listHits < 2 || !strings.Contains(out.String(), "买牛奶") {
		t.Fatalf("expected cache invalidation & new task in list (hits=%d): %s", listHits, out.String())
	}

	// 3) Complete task 202
	out.Reset()
	if code := runTasks([]string{"done", "202"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks done failed (%d): %s", code, errBuf.String())
	}
	if lastCompletePath != "/action/api/v2/tasks/202/complete" {
		t.Fatalf("unexpected complete path: %s", lastCompletePath)
	}

	// 4) Reopen task 202
	out.Reset()
	if code := runTasks([]string{"reopen", "202"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks reopen failed (%d): %s", code, errBuf.String())
	}
	if lastReopenPath != "/action/api/v2/tasks/202/reopen" {
		t.Fatalf("unexpected reopen path: %s", lastReopenPath)
	}

	// 5) Update only starred=false on 202: must preserve original content "买牛奶" and priority=0
	out.Reset()
	if code := runTasks([]string{"update", "202", "-star=false"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks update failed (%d): %s", code, errBuf.String())
	}
	if lastUpdateBody["content"] != "买牛奶" || lastUpdateBody["priority"] != float64(taskPriorityHigh) || lastUpdateBody["starred"] != false {
		t.Fatalf("update did not preserve fields properly: %#v", lastUpdateBody)
	}

	// 6) Delete task 202
	out.Reset()
	if code := runTasks([]string{"delete", "202"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("tasks delete failed (%d): %s", code, errBuf.String())
	}
	if lastDeleteQuery != "source=rayneoTasks" {
		t.Fatalf("unexpected delete query: %q", lastDeleteQuery)
	}
	if _, stillThere := tasks["202"]; stillThere {
		t.Fatalf("expected task 202 deleted")
	}
}

func TestFullFallbackToRecordingStdoutAndRichMarkdown(t *testing.T) {
	setupMockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/lifelog/api/v1/events/rec_abc/full":
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 1001, "msg": "event not found"})
		case "/clsapi/aiTask/getDataAllByTaskId":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": []map[string]any{
					{
						"aiTask": map[string]any{
							"taskId": "rec_abc", "taskType": "recording", "name": "地铁广播测试",
							"createTime": 1794648139043, "recordingDurationMs": 60000,
						},
						"recording": map[string]any{
							"taskId": "rec_abc", "audioDurationFormatted": "01:00",
							"sourceLanguageCode": "zh-CN",
							"marks": []map[string]any{
								{"markTime": 1794648159043, "markId": "m1", "remarks": []map[string]any{{"hashtag": "重点", "remark": "中心站"}}},
							},
							"outline": []map[string]any{
								{"timestamp": "1794648139043", "subtitle": "地铁报站开始"},
							},
							"clips": []map[string]any{
								{"speakerName": "我", "content": "下一站中心站", "startTime": 1794648139043, "endTime": 1794648145043},
							},
						},
						"summary": []map[string]any{
							{
								"model": "QWEN3_7_MAX",
								"content": map[string]any{
									"content": "这是一段地铁广播摘要。",
									"mindMap": map[string]any{
										"title": "地铁广播(测试)",
										"children": []map[string]any{
											{"title": "到站播报", "children": []map[string]any{{"title": "中心站", "children": []any{}}}},
										},
									},
								},
							},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})

	// Previously, `full -id <recordingId>` without `-out` silently produced empty stdout!
	var out, errBuf bytes.Buffer
	if code := runFull([]string{"-id", "rec_abc", "-format", "md"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("runFull failed (%d): %s", code, errBuf.String())
	}
	md := out.String()
	for _, want := range []string{"# 地铁广播测试", "## 重点打点（1）", "#重点 中心站", "## 提纲（1）", "地铁报站开始", "下一站中心站", "## AI 摘要", "## 思维导图", "```mermaid", "root((地铁广播（测试）))", "中心站"} {
		if !strings.Contains(md, want) {
			t.Errorf("expected %q in rendered markdown:\n%s", want, md)
		}
	}

	out.Reset()
	if code := runFull([]string{"-id", "rec_abc", "-format", "srt"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("runFull srt failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "00:00:00,000 --> 00:00:06,000") {
		t.Fatalf("expected SRT output, got: %s", out.String())
	}
}

func TestRecordingsPaginationAndDeviceFilter(t *testing.T) {
	pages := 0
	var seenDevices []any
	setupMockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seenDevices = append(seenDevices, body["deviceType"])
		pages++
		var rows []map[string]any
		if pages == 1 {
			rows = []map[string]any{
				{"taskId": "t1", "taskType": "recording", "name": "rec-1", "createTime": 2000, "recordingDurationMs": 10000},
				{"taskId": "t2", "taskType": "recording", "name": "rec-2", "createTime": 1000, "recordingDurationMs": 20000},
			}
		} else {
			rows = []map[string]any{
				{"taskId": "t3", "taskType": "recording", "name": "rec-3", "createTime": 500, "recordingDurationMs": 30000},
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "msg": "success",
			"data": map[string]any{"pageList": rows},
		})
	})

	var out, errBuf bytes.Buffer
	if code := runRecordings([]string{"-device", "", "-n", "2", "-all", "-json"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("runRecordings failed (%d): %s", code, errBuf.String())
	}
	var got []aiTaskRow
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal recordings json: %v\n%s", err, out.String())
	}
	if len(got) != 3 || pages != 2 {
		t.Fatalf("expected 3 rows across 2 pages, got %d rows / %d pages", len(got), pages)
	}
	if seenDevices[0] != nil {
		t.Fatalf("expected nil deviceType when -device='', got %#v", seenDevices[0])
	}
}

func TestTrailAndDigestAndReport(t *testing.T) {
	geoCalls := 0
	setupMockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/lifelog/api/v1/events":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"date": "2026-11-10", "timezone": "Asia/Shanghai",
					"events": []map[string]any{
						{
							"id": "ev1", "title": "项目架构讨论", "eventType": "audio", "flowType": "conversation",
							"startTimeFormatted": "10:00", "endTimeFormatted": "11:00",
							"audioLengthFormatted": "60:00", "audioLengthSeconds": 3600,
							"summaryContent": "讨论系统架构与接口方案",
						},
					},
					"trailPoints": []map[string]any{
						{
							"latitude": 31.23040, "longitude": 121.47370, "altitude": 12.5,
							"pointType": "stayPoint", "sourceEventId": "ev1",
							"timestamp": 1794276000000, "stayStartTime": 1794276000000, "stayEndTime": 1794279600000, "stayRadius": 20,
						},
						{
							"latitude": 31.23140, "longitude": 121.47470, "altitude": 13.0,
							"pointType": "trail", "timestamp": 1794280000000,
						},
					},
				},
			})
		case "/profileapi/geo/reverseGeocode":
			geoCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{"provinceName": "示例省", "cityName": "示例市", "districtName": "示例区"},
			})
		case "/lifelog/api/v1/journal/detail":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"title": "高效工作日", "periodStart": "2026-11-10", "visualTag": "focused", "mode": "panorama",
					"sections": []map[string]any{
						{"title": "上午评审", "blocks": []map[string]any{{"content": "顺利完成方案评审。"}}},
					},
				},
			})
		case "/action/api/v2/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"pending":   []map[string]any{{"id": "t1", "content": "整理评审文档", "createTime": "2026-11-10T11:30:00", "completed": false}},
					"completed": []map[string]any{},
				},
			})
		default:
			http.NotFound(w, r)
		}
	})

	// Test trail GPX + geocode + WGS-84
	var out, errBuf bytes.Buffer
	if code := runTrail([]string{"-date", "2026-11-10", "-format", "gpx", "-geocode", "-wgs84"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("trail gpx failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "<gpx ") || !strings.Contains(out.String(), "项目架构讨论") || !strings.Contains(out.String(), "示例省 示例市 示例区") {
		t.Fatalf("unexpected GPX output:\n%s", out.String())
	}
	if geoCalls != 1 {
		t.Fatalf("expected 1 reverseGeocode call, got %d", geoCalls)
	}

	// Test trail GeoJSON & KML
	out.Reset()
	if code := runTrail([]string{"-date", "2026-11-10", "-format", "geojson"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("trail geojson failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), `"FeatureCollection"`) || !strings.Contains(out.String(), `"LineString"`) {
		t.Fatalf("unexpected GeoJSON output:\n%s", out.String())
	}

	out.Reset()
	if code := runTrail([]string{"-date", "2026-11-10", "-format", "kml"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("trail kml failed (%d): %s", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "<kml ") {
		t.Fatalf("unexpected KML output:\n%s", out.String())
	}

	// Test digest writing into directory => <dir>/2026-11-10.md
	outDir := t.TempDir()
	out.Reset()
	if code := runDigest([]string{"-date", "2026-11-10", "-geocode", "-out", outDir}, false, &out, &errBuf); code != 0 {
		t.Fatalf("digest failed (%d): %s", code, errBuf.String())
	}
	digestBytes, err := os.ReadFile(filepath.Join(outDir, "2026-11-10.md"))
	if err != nil {
		t.Fatalf("read digest file: %v", err)
	}
	digestMD := string(digestBytes)
	for _, want := range []string{"date: 2026-11-10", `title: "高效工作日"`, "mood: \"focused\"", "项目架构讨论", "示例省 示例市 示例区", "整理评审文档"} {
		if !strings.Contains(digestMD, want) {
			t.Errorf("missing %q in digest:\n%s", want, digestMD)
		}
	}

	// Test report retains journal title in TOC and heading
	repDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(repDir, "journal-2026-11-10.md"), []byte("# 高效工作日\n\n正文内容"), 0o644)
	repOut := filepath.Join(repDir, "REPORT.md")
	if err := buildReport(repDir, repOut, "测试汇总"); err != nil {
		t.Fatalf("buildReport: %v", err)
	}
	repBytes, _ := os.ReadFile(repOut)
	if !strings.Contains(string(repBytes), "- [2026-11-10 · 高效工作日](#2026-11-10)") || !strings.Contains(string(repBytes), "## 2026-11-10 · 高效工作日") {
		t.Fatalf("expected title in report TOC and heading, got:\n%s", string(repBytes))
	}

	// Test resolveDate shorthands
	if got := resolveDate("today"); got != time.Now().Format("2006-01-02") {
		t.Fatalf("resolveDate(today) = %s", got)
	}
	if got := resolveDate("yesterday"); got != time.Now().AddDate(0, 0, -1).Format("2006-01-02") {
		t.Fatalf("resolveDate(yesterday) = %s", got)
	}
	if got := resolveDate("-3d"); got != time.Now().AddDate(0, 0, -3).Format("2006-01-02") {
		t.Fatalf("resolveDate(-3d) = %s", got)
	}
}

func TestChatMessagesAndAccountCommands(t *testing.T) {
	setupMockGateway(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/clsapi/aiChatMsg/selectPage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": []map[string]any{
					{
						"msgId": "m2", "role": "assistant", "model": "QWEN3_7_MAX", "msgSource": "glass", "createTime": 2000,
						"content": map[string]any{"text": "<thought>先分析问题</thought>这是正式回答。"},
					},
					{
						"msgId": "m1", "role": "user", "msgSource": "glass", "createTime": 1000,
						"content": map[string]any{"text": "帮我分析一下"},
					},
				},
			})
		case "/factsapi/api/v1/facts/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"confirmedList": []map[string]any{{"id": "f1", "content": "软件工程开发者", "category": "background"}},
					"proposedList":  []map[string]any{},
				},
			})
		case "/identify/device/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"deviceName": "RayNeo X3-TEST", "deviceType": "venus", "sn": "SN0000000001", "macAddr": "001122334455", "glassesDeviceId": "dev1"}},
			})
		case "/profileapi/v1/profile/query":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{"accountId": "100000000000001", "alwaysOnSummaryStyle": "panorama", "autoTranscriptionEnabled": true},
			})
		case "/lifelog/api/v1/settings/onboarding":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{"onboarded": true, "enabled": true, "selfIntro": "示例个人简介", "futureGoal": []string{"时间管理"}},
			})
		case "/balance/members":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{
					"memberLevel": "STANDARD", "effectTime": 1793682666581, "expireTime": 8640000000000000,
					"resourcePackages": []map[string]any{
						{"resourceType": "TOPIC", "desc": "专题数", "used": 1, "balance": 2, "total": 3, "unitKey": "个"},
					},
				},
			})
		case "/balance/usages":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "msg": "success",
				"data": map[string]any{"total": 0, "records": []any{}},
			})
		default:
			http.NotFound(w, r)
		}
	})

	var out, errBuf bytes.Buffer
	if code := runChat([]string{"-id", "chat123", "-md"}, false, &out, &errBuf); code != 0 {
		t.Fatalf("chat -id failed (%d): %s", code, errBuf.String())
	}
	chatMD := out.String()
	// Verify chronological ordering: user (1000) before assistant (2000), and <thought> converted to <details>.
	if idxUser, idxAsst := strings.Index(chatMD, "帮我分析一下"), strings.Index(chatMD, "这是正式回答"); idxUser < 0 || idxAsst < 0 || idxUser > idxAsst {
		t.Fatalf("messages not chronologically ordered:\n%s", chatMD)
	}
	if !strings.Contains(chatMD, "<summary>思考过程</summary>") {
		t.Fatalf("expected thought block rendered as details:\n%s", chatMD)
	}

	out.Reset()
	if code := runFacts([]string{"-md"}, false, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "软件工程开发者") {
		t.Fatalf("facts -md failed (%d): %s %s", code, out.String(), errBuf.String())
	}

	out.Reset()
	if code := runDevices([]string{"-table"}, false, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "RayNeo X3-TEST") {
		t.Fatalf("devices -table failed (%d): %s %s", code, out.String(), errBuf.String())
	}

	out.Reset()
	if code := runProfile([]string{"-onboarding", "-md"}, false, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "示例个人简介") {
		t.Fatalf("profile -onboarding -md failed (%d): %s %s", code, out.String(), errBuf.String())
	}

	out.Reset()
	if code := runQuota([]string{"-usages", "-table"}, false, &out, &errBuf); code != 0 || !strings.Contains(out.String(), "STANDARD") || !strings.Contains(out.String(), "TOPIC") {
		t.Fatalf("quota -usages -table failed (%d): %s %s", code, out.String(), errBuf.String())
	}
}
