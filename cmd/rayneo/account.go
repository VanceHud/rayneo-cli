package main

import (
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
)

// ---------- facts: GET /factsapi/api/v1/facts/list ----------

func cmdFacts(args []string, debug bool) {
	if code := runFacts(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runFacts(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("facts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	format := fs.String("format", "json", "json|md")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	status := fs.String("status", "2", "facts status filter (default 2)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *mdFlag {
		fmtMode = "md"
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	q := params(kv)
	if q.Get("status") == "" && strings.TrimSpace(*status) != "" {
		q.Set("status", strings.TrimSpace(*status))
	}
	resp, body, err := callWithAuth(cl, "GET", "/factsapi/api/v1/facts/list", q, nil)
	if err != nil {
		fmt.Fprintln(stderr, "facts:", err)
		return 1
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		if fmtMode == "json" {
			printJSONTo(stdout, body)
		}
		return 1
	}
	if fmtMode == "json" {
		printJSONTo(stdout, body)
		return 0
	}
	if fmtMode != "md" {
		fmt.Fprintf(stderr, "facts: invalid -format %q (use json|md)\n", *format)
		return 2
	}

	var env struct {
		Code int `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ConfirmedList []map[string]any `json:"confirmedList"`
			ProposedList  []map[string]any `json:"proposedList"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Code != 0 {
		printJSONTo(stdout, body)
		fmt.Fprintf(stderr, "facts: decode error (code=%d msg=%s)\n", env.Code, env.Msg)
		return 1
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# 事实记忆卡（已确认 %d · 待审核 %d）\n\n", len(env.Data.ConfirmedList), len(env.Data.ProposedList))
	renderList := func(title string, items []map[string]any) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&sb, "## %s（%d）\n\n", title, len(items))
		for _, it := range items {
			text := firstNonEmpty(asString(it["content"]), asString(it["fact"]), asString(it["title"]), asString(it["summary"]))
			cat := firstNonEmpty(asString(it["category"]), asString(it["type"]))
			id := firstNonEmpty(asString(it["id"]), asString(it["factId"]))
			line := "- " + orDefault(text, "(empty)")
			if cat != "" {
				line += " _(" + cat + ")_"
			}
			if id != "" {
				line += " · `" + id + "`"
			}
			sb.WriteString(line + "\n")
		}
		sb.WriteString("\n")
	}
	renderList("已确认", env.Data.ConfirmedList)
	renderList("待审核", env.Data.ProposedList)
	if len(env.Data.ConfirmedList) == 0 && len(env.Data.ProposedList) == 0 {
		sb.WriteString("_（暂无事实卡记录）_\n")
	}
	fmt.Fprint(stdout, sb.String())
	return 0
}

// ---------- devices: GET /identify/device/list ----------

func cmdDevices(args []string, debug bool) {
	if code := runDevices(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runDevices(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	format := fs.String("format", "json", "json|table|md")
	tableFlag := fs.Bool("table", false, "shorthand for -format table")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *tableFlag {
		fmtMode = "table"
	} else if *mdFlag {
		fmtMode = "md"
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	resp, body, err := callWithAuth(cl, "GET", "/identify/device/list", params(kv), nil)
	if err != nil {
		fmt.Fprintln(stderr, "devices:", err)
		return 1
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		if fmtMode == "json" {
			printJSONTo(stdout, body)
		}
		return 1
	}
	if fmtMode == "json" {
		printJSONTo(stdout, body)
		return 0
	}

	var env struct {
		Data []struct {
			DeviceName      string `json:"deviceName"`
			DeviceType      string `json:"deviceType"`
			GlassesDeviceID string `json:"glassesDeviceId"`
			MacAddr         string `json:"macAddr"`
			SN              string `json:"sn"`
			MfiSerialNumber string `json:"mfiSerialNumber"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		printJSONTo(stdout, body)
		return 1
	}

	switch fmtMode {
	case "table":
		fmt.Fprintf(stdout, "%-20s %-10s %-18s %-14s %s\n", "NAME", "TYPE", "SN", "MAC", "GLASSES_DEVICE_ID")
		for _, d := range env.Data {
			sn := firstNonEmpty(d.SN, d.MfiSerialNumber, "-")
			fmt.Fprintf(stdout, "%-20s %-10s %-18s %-14s %s\n",
				orDefault(d.DeviceName, "-"), orDefault(d.DeviceType, "-"), sn, orDefault(d.MacAddr, "-"), orDefault(d.GlassesDeviceID, "-"))
		}
		fmt.Fprintf(stdout, "\n共 %d 台设备\n", len(env.Data))
	case "md":
		fmt.Fprintf(stdout, "# 已绑定设备（%d）\n\n", len(env.Data))
		for _, d := range env.Data {
			sn := firstNonEmpty(d.SN, d.MfiSerialNumber, "-")
			fmt.Fprintf(stdout, "- **%s** (`%s`) · SN `%s` · MAC `%s` · ID `%s`\n",
				orDefault(d.DeviceName, "未命名设备"), orDefault(d.DeviceType, "-"), sn, orDefault(d.MacAddr, "-"), orDefault(d.GlassesDeviceID, "-"))
		}
		if len(env.Data) == 0 {
			fmt.Fprintln(stdout, "_（暂无绑定设备）_")
		}
	default:
		fmt.Fprintf(stderr, "devices: invalid -format %q (use json|table|md)\n", *format)
		return 2
	}
	return 0
}

// ---------- profile: GET /profileapi/v1/profile/query (+ /lifelog/api/v1/settings/onboarding) ----------

func cmdProfile(args []string, debug bool) {
	if code := runProfile(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runProfile(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	onboarding := fs.Bool("onboarding", false, "also fetch /lifelog/api/v1/settings/onboarding (persona, selfIntro, goals)")
	format := fs.String("format", "json", "json|md")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *mdFlag {
		fmtMode = "md"
	}

	c := mustConfig()
	cl := newClient(c, debug, headers(hs))
	q := params(kv)
	if q.Get("accountId") == "" && c.AccountID != 0 {
		q.Set("accountId", fmt.Sprintf("%d", c.AccountID))
	}
	resp, pBody, err := callWithAuth(cl, "GET", "/profileapi/v1/profile/query", q, nil)
	if err != nil {
		fmt.Fprintln(stderr, "profile:", err)
		return 1
	}
	if authExpired(pBody) || resp.StatusCode == http.StatusUnauthorized {
		if fmtMode == "json" {
			printJSONTo(stdout, pBody)
		}
		return 1
	}

	var oBody []byte
	if *onboarding {
		_, ob, oerr := callWithAuth(cl, "GET", "/lifelog/api/v1/settings/onboarding", nil, nil)
		if oerr != nil {
			fmt.Fprintln(stderr, "profile onboarding:", oerr)
			return 1
		}
		if authExpired(ob) {
			return 1
		}
		oBody = ob
	}

	if fmtMode == "json" {
		if !*onboarding {
			printJSONTo(stdout, pBody)
			return 0
		}
		var pv, ov any
		_ = json.Unmarshal(pBody, &pv)
		_ = json.Unmarshal(oBody, &ov)
		out, _ := json.MarshalIndent(map[string]any{"profile": pv, "onboarding": ov}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	if fmtMode != "md" {
		fmt.Fprintf(stderr, "profile: invalid -format %q (use json|md)\n", *format)
		return 2
	}

	var pEnv struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(pBody, &pEnv)
	d := pEnv.Data
	var sb strings.Builder
	sb.WriteString("# RayNeo 账号档案与智记设置\n\n")
	if d != nil {
		fmt.Fprintf(&sb, "- **Account ID**：`%s`\n", asString(d["accountId"]))
		fmt.Fprintf(&sb, "- **每日故事风格**：`%s`\n", asString(d["alwaysOnSummaryStyle"]))
		fmt.Fprintf(&sb, "- **自动转录**：`%v`\n", d["autoTranscriptionEnabled"])
		fmt.Fprintf(&sb, "- **自动生成事实卡**：`%v`\n", d["factsAutoGenerate"])
		fmt.Fprintf(&sb, "- **AI 建议**：enabled=`%v` · pushToGlasses=`%v`\n", d["suggestEnabled"], d["suggestPushToGlasses"])
		fmt.Fprintf(&sb, "- **专题功能**：enabled=`%v` · recommend=`%v`\n", d["topicEnabled"], d["topicRecommend"])
		fmt.Fprintf(&sb, "- **勿扰模式**：`%v`\n", d["doNotDisturb"])
	}
	if len(oBody) > 0 {
		var oEnv struct {
			Data struct {
				Onboarded          bool     `json:"onboarded"`
				Enabled            bool     `json:"enabled"`
				CommonLanguage     []string `json:"commonLanguage"`
				SelfIntro          string   `json:"selfIntro"`
				FutureGoal         []string `json:"futureGoal"`
				SummaryStyle       string   `json:"summaryStyle"`
				MicPermission      bool     `json:"micPermission"`
				LocationPermission bool     `json:"locationPermission"`
			} `json:"data"`
		}
		if json.Unmarshal(oBody, &oEnv) == nil {
			o := oEnv.Data
			sb.WriteString("\n## 智记引导画像（Onboarding）\n\n")
			fmt.Fprintf(&sb, "- **已完成引导**：`%v` · **智记总开关**：`%v`\n", o.Onboarded, o.Enabled)
			if len(o.CommonLanguage) > 0 {
				fmt.Fprintf(&sb, "- **常用语言**：%s\n", strings.Join(o.CommonLanguage, ", "))
			}
			if o.SelfIntro != "" {
				fmt.Fprintf(&sb, "- **自我介绍**：%s\n", o.SelfIntro)
			}
			if len(o.FutureGoal) > 0 {
				fmt.Fprintf(&sb, "- **关注目标**：%s\n", strings.Join(o.FutureGoal, "、"))
			}
			fmt.Fprintf(&sb, "- **权限状态**：mic=`%v` · location=`%v`\n", o.MicPermission, o.LocationPermission)
		}
	}
	fmt.Fprint(stdout, sb.String())
	return 0
}

// ---------- quota: GET /balance/members & GET /balance/usages ----------

func cmdQuota(args []string, debug bool) {
	if code := runQuota(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runQuota(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("quota", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var kv, hs kvFlag
	fs.Var(&kv, "q", "query param key=value")
	fs.Var(&hs, "H", "extra header")
	usages := fs.Bool("usages", false, "also fetch consumption history via GET /balance/usages")
	page := fs.Int("page", 1, "usages page number")
	size := fs.Int("size", 20, "usages page size")
	resType := fs.String("resource-type", "", "optional filter for /balance/usages (ADVANCED_MODEL|TOPIC|RAYNEO_CLOUD)")
	format := fs.String("format", "json", "json|table|md")
	tableFlag := fs.Bool("table", false, "shorthand for -format table")
	mdFlag := fs.Bool("md", false, "shorthand for -format md")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fmtMode := strings.ToLower(strings.TrimSpace(*format))
	if *tableFlag {
		fmtMode = "table"
	} else if *mdFlag {
		fmtMode = "md"
	}

	cl := newClient(mustConfig(), debug, headers(hs))
	resp, mBody, err := callWithAuth(cl, "GET", "/balance/members", params(kv), nil)
	if err != nil {
		fmt.Fprintln(stderr, "quota:", err)
		return 1
	}
	if authExpired(mBody) || resp.StatusCode == http.StatusUnauthorized {
		if fmtMode == "json" {
			printJSONTo(stdout, mBody)
		}
		return 1
	}

	var uBody []byte
	if *usages {
		uq := url.Values{
			"page": {strconv.Itoa(*page)},
			"size": {strconv.Itoa(*size)},
		}
		if rt := strings.TrimSpace(*resType); rt != "" {
			uq.Set("resourceType", rt)
		}
		_, ub, uerr := callWithAuth(cl, "GET", "/balance/usages", uq, nil)
		if uerr != nil {
			fmt.Fprintln(stderr, "quota usages:", uerr)
			return 1
		}
		if authExpired(ub) {
			return 1
		}
		uBody = ub
	}

	if fmtMode == "json" {
		if !*usages {
			printJSONTo(stdout, mBody)
			return 0
		}
		var mv, uv any
		_ = json.Unmarshal(mBody, &mv)
		_ = json.Unmarshal(uBody, &uv)
		out, _ := json.MarshalIndent(map[string]any{"members": mv, "usages": uv}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}

	var mEnv struct {
		Code int `json:"code"`
		Data struct {
			MemberLevel      string `json:"memberLevel"`
			EffectTime       int64  `json:"effectTime"`
			ExpireTime       int64  `json:"expireTime"`
			ResourcePackages []struct {
				ResourceType string  `json:"resourceType"`
				Desc         string  `json:"desc"`
				Used         float64 `json:"used"`
				Balance      float64 `json:"balance"`
				Total        float64 `json:"total"`
				Infinite     bool    `json:"infinite"`
				UnitKey      string  `json:"unitKey"`
			} `json:"resourcePackages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(mBody, &mEnv); err != nil {
		printJSONTo(stdout, mBody)
		return 1
	}

	fmtTS := func(ms int64) string {
		if ms <= 0 {
			return "-"
		}
		if ms >= 4_000_000_000_000 { // sentinel permanent timestamp (e.g. 8640000000000000)
			return "永久"
		}
		return time.UnixMilli(ms).Format("2006-01-02 15:04")
	}
	fmtAmount := func(v float64, unit string) string {
		if strings.EqualFold(unit, "B") {
			const (
				kb = 1024.0
				mb = 1024.0 * kb
				gb = 1024.0 * mb
			)
			switch {
			case v >= gb:
				return fmt.Sprintf("%.2f GB", v/gb)
			case v >= mb:
				return fmt.Sprintf("%.1f MB", v/mb)
			case v >= kb:
				return fmt.Sprintf("%.1f KB", v/kb)
			default:
				return fmt.Sprintf("%.0f B", v)
			}
		}
		if unit == "" {
			return fmt.Sprintf("%.0f", v)
		}
		return fmt.Sprintf("%g %s", v, unit)
	}

	switch fmtMode {
	case "table":
		fmt.Fprintf(stdout, "会员等级 : %s\n生效时间 : %s\n到期时间 : %s\n\n",
			orDefault(mEnv.Data.MemberLevel, "-"), fmtTS(mEnv.Data.EffectTime), fmtTS(mEnv.Data.ExpireTime))
		fmt.Fprintf(stdout, "%-16s %-12s %-12s %-12s %s\n", "RESOURCE", "USED", "BALANCE", "TOTAL", "DESC")
		for _, p := range mEnv.Data.ResourcePackages {
			used := fmtAmount(p.Used, p.UnitKey)
			bal := fmtAmount(p.Balance, p.UnitKey)
			tot := fmtAmount(p.Total, p.UnitKey)
			if p.Infinite {
				bal = "∞"
				tot = "∞"
			}
			fmt.Fprintf(stdout, "%-16s %-12s %-12s %-12s %s\n",
				p.ResourceType, used, bal, tot, p.Desc)
		}
	case "md":
		fmt.Fprintf(stdout, "# 会员与额度概览\n\n")
		fmt.Fprintf(stdout, "**等级** `%s` · **生效** %s · **到期** %s\n\n",
			orDefault(mEnv.Data.MemberLevel, "-"), fmtTS(mEnv.Data.EffectTime), fmtTS(mEnv.Data.ExpireTime))
		if len(mEnv.Data.ResourcePackages) > 0 {
			fmt.Fprintln(stdout, "| 资源 | 已用 | 剩余 | 总额 | 说明 |")
			fmt.Fprintln(stdout, "|---|---:|---:|---:|---|")
			for _, p := range mEnv.Data.ResourcePackages {
				used := fmtAmount(p.Used, p.UnitKey)
				bal := fmtAmount(p.Balance, p.UnitKey)
				tot := fmtAmount(p.Total, p.UnitKey)
				if p.Infinite {
					bal = "∞"
					tot = "∞"
				}
				fmt.Fprintf(stdout, "| `%s` | %s | %s | %s | %s |\n",
					p.ResourceType, used, bal, tot, p.Desc)
			}
		}
	default:
		fmt.Fprintf(stderr, "quota: invalid -format %q (use json|table|md)\n", *format)
		return 2
	}

	if len(uBody) > 0 {
		var uEnv struct {
			Data struct {
				Total   int              `json:"total"`
				Records []map[string]any `json:"records"`
			} `json:"data"`
		}
		if json.Unmarshal(uBody, &uEnv) == nil {
			fmt.Fprintf(stdout, "\n用量记录：共 %d 条（当前页 %d 条）\n", uEnv.Data.Total, len(uEnv.Data.Records))
			for _, rec := range uEnv.Data.Records {
				line, _ := json.Marshal(rec)
				fmt.Fprintln(stdout, " -", string(line))
			}
		}
	}
	return 0
}
