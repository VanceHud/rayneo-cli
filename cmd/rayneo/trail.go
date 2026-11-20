package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/VanceHud/rayneo-cli/internal/api"
)

// resolveDate expands "today", "yesterday", or "-Nd" relative shorthands into YYYY-MM-DD.
func resolveDate(s string) string {
	s = strings.TrimSpace(s)
	now := time.Now()
	switch strings.ToLower(s) {
	case "", "today":
		return now.Format("2006-01-02")
	case "yesterday":
		return now.AddDate(0, 0, -1).Format("2006-01-02")
	}
	if strings.HasPrefix(s, "-") && strings.HasSuffix(strings.ToLower(s), "d") {
		if n, err := strconv.Atoi(s[1 : len(s)-1]); err == nil && n >= 0 && n <= 3650 {
			return now.AddDate(0, 0, -n).Format("2006-01-02")
		}
	}
	return s
}

type trailPoint struct {
	Latitude         float64  `json:"latitude"`
	Longitude        float64  `json:"longitude"`
	Altitude         *float64 `json:"altitude"`
	Accuracy         *float64 `json:"accuracy"`
	Bearing          *float64 `json:"bearing"`
	Speed            *float64 `json:"speed"`
	PointType        string   `json:"pointType"`
	SourceEventID    string   `json:"sourceEventId"`
	Timestamp        int64    `json:"timestamp"`
	StayStartTime    *int64   `json:"stayStartTime"`
	StayEndTime      *int64   `json:"stayEndTime"`
	StayRadius       *float64 `json:"stayRadius"`
	HiddenPointCount *int     `json:"hiddenPointCount"`
	IsFirst          bool     `json:"isFirst"`
	Timezone         string   `json:"timezone"`
}

type trailStay struct {
	Index      int     `json:"index"`
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	Altitude   float64 `json:"altitude,omitempty"`
	StartTime  int64   `json:"startTime"`
	EndTime    int64   `json:"endTime"`
	DurationS  int64   `json:"durationSeconds"`
	RadiusM    float64 `json:"stayRadius,omitempty"`
	EventID    string  `json:"sourceEventId,omitempty"`
	EventTitle string  `json:"eventTitle,omitempty"`
	Location   string  `json:"location,omitempty"`
}

type trailBundle struct {
	Date         string       `json:"date"`
	Timezone     string       `json:"timezone"`
	CoordSystem  string       `json:"coordSystem"`
	PointCount   int          `json:"pointCount"`
	StayCount    int          `json:"stayCount"`
	DistanceKm   float64      `json:"distanceKm"`
	Stays        []trailStay  `json:"stays"`
	Points       []trailPoint `json:"points"`
	EventTitles  map[string]string `json:"-"`
}

type dayEventsEnvelope struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Date        string           `json:"date"`
		Timezone    string           `json:"timezone"`
		Events      []map[string]any `json:"events"`
		TrailPoints []trailPoint     `json:"trailPoints"`
	} `json:"data"`
}

func fetchDayEventsRaw(cl *api.Client, date, tz string) ([]byte, *dayEventsEnvelope, error) {
	q := url.Values{"date": {date}, "timezone": {tz}}
	resp, body, err := callWithAuth(cl, "GET", "/lifelog/api/v1/events", q, nil)
	if err != nil {
		return nil, nil, err
	}
	if authExpired(body) || resp.StatusCode == http.StatusUnauthorized {
		return body, nil, fmt.Errorf("authentication expired")
	}
	var env dayEventsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return body, nil, err
	}
	if env.Code != 0 {
		return body, &env, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return body, &env, nil
}

func buildTrailBundle(env *dayEventsEnvelope, date, tz string, toWGS84 bool) *trailBundle {
	if env.Data.Date != "" {
		date = env.Data.Date
	}
	if env.Data.Timezone != "" {
		tz = env.Data.Timezone
	}
	titles := map[string]string{}
	for _, e := range env.Data.Events {
		if id := asString(e["id"]); id != "" {
			titles[id] = asString(e["title"])
		}
	}
	pts := make([]trailPoint, len(env.Data.TrailPoints))
	copy(pts, env.Data.TrailPoints)
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })

	coordSys := "GCJ-02/raw"
	if toWGS84 {
		coordSys = "WGS-84"
		for i := range pts {
			pts[i].Latitude, pts[i].Longitude = gcj02ToWGS84(pts[i].Latitude, pts[i].Longitude)
		}
	}

	var stays []trailStay
	for _, p := range pts {
		if p.PointType != "stayPoint" && p.StayStartTime == nil && p.StayEndTime == nil {
			continue
		}
		st := p.Timestamp
		if p.StayStartTime != nil && *p.StayStartTime > 0 {
			st = *p.StayStartTime
		}
		et := st
		if p.StayEndTime != nil && *p.StayEndTime > st {
			et = *p.StayEndTime
		}
		durS := (et - st) / 1000
		if durS < 0 {
			durS = 0
		}
		alt := 0.0
		if p.Altitude != nil {
			alt = *p.Altitude
		}
		rad := 0.0
		if p.StayRadius != nil {
			rad = *p.StayRadius
		}
		stays = append(stays, trailStay{
			Index:      len(stays) + 1,
			Latitude:   p.Latitude,
			Longitude:  p.Longitude,
			Altitude:   alt,
			StartTime:  st,
			EndTime:    et,
			DurationS:  durS,
			RadiusM:    rad,
			EventID:    p.SourceEventID,
			EventTitle: titles[p.SourceEventID],
		})
	}

	// Calculate total Haversine distance over chronologically ordered points.
	totalKm := 0.0
	for i := 1; i < len(pts); i++ {
		d := haversineKm(pts[i-1].Latitude, pts[i-1].Longitude, pts[i].Latitude, pts[i].Longitude)
		if d > 0 && d < 50 { // ignore implausible single-hop teleport glitches
			totalKm += d
		}
	}

	return &trailBundle{
		Date:        date,
		Timezone:    tz,
		CoordSystem: coordSys,
		PointCount:  len(pts),
		StayCount:   len(stays),
		DistanceKm:  math.Round(totalKm*100) / 100,
		Stays:       stays,
		Points:      pts,
		EventTitles: titles,
	}
}

func reverseGeocode(cl *api.Client, lat, lon float64) (string, error) {
	q := url.Values{
		"latitude":  {fmt.Sprintf("%.6f", lat)},
		"longitude": {fmt.Sprintf("%.6f", lon)},
	}
	_, body, err := callWithAuth(cl, "GET", "/profileapi/geo/reverseGeocode", q, nil)
	if err != nil {
		return "", err
	}
	var r struct {
		Code int `json:"code"`
		Data struct {
			CountryName  string `json:"countryName"`
			ProvinceName string `json:"provinceName"`
			CityName     string `json:"cityName"`
			DistrictName string `json:"districtName"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Code != 0 {
		return "", fmt.Errorf("reverseGeocode failed")
	}
	var parts []string
	seen := map[string]bool{}
	for _, p := range []string{r.Data.ProvinceName, r.Data.CityName, r.Data.DistrictName} {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 && strings.TrimSpace(r.Data.CountryName) != "" {
		return strings.TrimSpace(r.Data.CountryName), nil
	}
	return strings.Join(parts, " "), nil
}

// enrichStayLocations resolves stay coordinates via /profileapi/geo/reverseGeocode
// using the original (pre-WGS84) coordinates, deduplicated to ~100m cells.
func enrichStayLocations(cl *api.Client, tb *trailBundle, rawEnv *dayEventsEnvelope) {
	if cl == nil || tb == nil || len(tb.Stays) == 0 {
		return
	}
	// Map original coordinates by stay index in case tb was converted to WGS-84.
	rawStays := buildTrailBundle(rawEnv, tb.Date, tb.Timezone, false).Stays
	memo := map[string]string{}
	calls := 0
	for i := range tb.Stays {
		lat, lon := tb.Stays[i].Latitude, tb.Stays[i].Longitude
		if i < len(rawStays) {
			lat, lon = rawStays[i].Latitude, rawStays[i].Longitude
		}
		key := fmt.Sprintf("%.3f,%.3f", lat, lon)
		if loc, ok := memo[key]; ok {
			tb.Stays[i].Location = loc
			continue
		}
		if calls > 0 {
			time.Sleep(150 * time.Millisecond)
		}
		calls++
		loc, err := reverseGeocode(cl, lat, lon)
		if err == nil && loc != "" {
			memo[key] = loc
			tb.Stays[i].Location = loc
		}
	}
}

func cmdTrail(args []string, debug bool) {
	if code := runTrail(args, debug, os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

func runTrail(args []string, debug bool, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("trail", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dateFlag := fs.String("date", "today", "YYYY-MM-DD, today, yesterday, or -Nd")
	tz := fs.String("tz", "Asia/Shanghai", "timezone")
	format := fs.String("format", "stays", "stays|gpx|geojson|kml|json")
	out := fs.String("out", "", "write output to file path instead of stdout")
	geocode := fs.Bool("geocode", false, "resolve stay locations via /profileapi/geo/reverseGeocode")
	wgs84 := fs.Bool("wgs84", false, "convert China GCJ-02 coordinates to WGS-84 (for Google Earth / Apple Maps)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	day := resolveDate(*dateFlag)
	cl := newClient(mustConfig(), debug, nil)
	_, env, err := fetchDayEventsRaw(cl, day, *tz)
	if err != nil {
		fmt.Fprintln(stderr, "trail:", err)
		return 1
	}

	tb := buildTrailBundle(env, day, *tz, *wgs84)
	if *geocode && len(tb.Stays) > 0 {
		enrichStayLocations(cl, tb, env)
	}

	var rendered string
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "stays", "md":
		rendered = tb.MarkdownStays()
	case "gpx":
		rendered = tb.GPX()
	case "geojson":
		rendered = tb.GeoJSON()
	case "kml":
		rendered = tb.KML()
	case "json":
		b, _ := json.MarshalIndent(tb, "", "  ")
		rendered = string(b) + "\n"
	default:
		fmt.Fprintf(stderr, "trail: invalid -format %q (use stays|gpx|geojson|kml|json)\n", *format)
		return 2
	}

	if *out != "" {
		if dir := filepath.Dir(*out); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintln(stderr, "trail:", err)
				return 1
			}
		}
		if err := os.WriteFile(*out, []byte(rendered), 0o644); err != nil {
			fmt.Fprintln(stderr, "trail:", err)
			return 1
		}
		fmt.Fprintf(stdout, "trail written: %s (%d points, %d stays, %.2f km)\n", *out, tb.PointCount, tb.StayCount, tb.DistanceKm)
		return 0
	}
	fmt.Fprint(stdout, rendered)
	return 0
}

func formatDurationHuman(sec int64) string {
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	h := sec / 3600
	m := (sec % 3600) / 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

func (tb *trailBundle) loc() *time.Location {
	if l, err := time.LoadLocation(tb.Timezone); err == nil {
		return l
	}
	return time.Local
}

func (tb *trailBundle) MarkdownStays() string {
	var b strings.Builder
	loc := tb.loc()
	fmt.Fprintf(&b, "# %s 行程轨迹与停留点\n\n", tb.Date)
	fmt.Fprintf(&b, "**轨迹点** %d · **停留点** %d · **估算里程** %.2f km · **坐标系** %s\n\n",
		tb.PointCount, tb.StayCount, tb.DistanceKm, tb.CoordSystem)
	if len(tb.Stays) == 0 {
		if tb.PointCount == 0 {
			b.WriteString("_（该日期无 GPS 轨迹记录）_\n")
		} else {
			b.WriteString("_（有轨迹点但无聚合停留点）_\n")
		}
		return b.String()
	}
	b.WriteString("| # | 时段 | 停留 | 坐标 | 地区 | 关联事件 |\n")
	b.WriteString("|---:|---|---:|---|---|---|\n")
	for _, s := range tb.Stays {
		startStr := time.UnixMilli(s.StartTime).In(loc).Format("15:04")
		endStr := time.UnixMilli(s.EndTime).In(loc).Format("15:04")
		place := orDefault(s.Location, "-")
		ev := orDefault(s.EventTitle, "-")
		fmt.Fprintf(&b, "| %d | %s–%s | %s | `%.5f, %.5f` | %s | %s |\n",
			s.Index, startStr, endStr, formatDurationHuman(s.DurationS), s.Latitude, s.Longitude, place, ev)
	}
	return b.String()
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (tb *trailBundle) GPX() string {
	var b strings.Builder
	loc := tb.loc()
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<gpx version="1.1" creator="rayneo-cli" xmlns="http://www.topografix.com/GPX/1/1">` + "\n")
	fmt.Fprintf(&b, "  <metadata>\n    <name>RayNeo Lifelog %s</name>\n    <desc>Points: %d, Stays: %d, Distance: %.2f km (%s)</desc>\n  </metadata>\n",
		xmlEscape(tb.Date), tb.PointCount, tb.StayCount, tb.DistanceKm, xmlEscape(tb.CoordSystem))
	for _, s := range tb.Stays {
		name := fmt.Sprintf("Stay #%d (%s)", s.Index, formatDurationHuman(s.DurationS))
		if s.EventTitle != "" {
			name = s.EventTitle
		}
		var descParts []string
		descParts = append(descParts, fmt.Sprintf("%s-%s (%s)",
			time.UnixMilli(s.StartTime).In(loc).Format("15:04"),
			time.UnixMilli(s.EndTime).In(loc).Format("15:04"),
			formatDurationHuman(s.DurationS)))
		if s.Location != "" {
			descParts = append(descParts, s.Location)
		}
		fmt.Fprintf(&b, "  <wpt lat=\"%.7f\" lon=\"%.7f\">\n", s.Latitude, s.Longitude)
		if s.Altitude != 0 {
			fmt.Fprintf(&b, "    <ele>%.2f</ele>\n", s.Altitude)
		}
		if s.StartTime > 0 {
			fmt.Fprintf(&b, "    <time>%s</time>\n", time.UnixMilli(s.StartTime).UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "    <name>%s</name>\n", xmlEscape(name))
		fmt.Fprintf(&b, "    <desc>%s</desc>\n", xmlEscape(strings.Join(descParts, " · ")))
		b.WriteString("  </wpt>\n")
	}
	if len(tb.Points) > 0 {
		fmt.Fprintf(&b, "  <trk>\n    <name>RayNeo Trail %s</name>\n    <trkseg>\n", xmlEscape(tb.Date))
		for _, p := range tb.Points {
			fmt.Fprintf(&b, "      <trkpt lat=\"%.7f\" lon=\"%.7f\">", p.Latitude, p.Longitude)
			if p.Altitude != nil {
				fmt.Fprintf(&b, "<ele>%.2f</ele>", *p.Altitude)
			}
			if p.Timestamp > 0 {
				fmt.Fprintf(&b, "<time>%s</time>", time.UnixMilli(p.Timestamp).UTC().Format(time.RFC3339))
			}
			b.WriteString("</trkpt>\n")
		}
		b.WriteString("    </trkseg>\n  </trk>\n")
	}
	b.WriteString("</gpx>\n")
	return b.String()
}

func (tb *trailBundle) GeoJSON() string {
	features := make([]map[string]any, 0, len(tb.Stays)+1)
	if len(tb.Points) >= 2 {
		coords := make([][]float64, 0, len(tb.Points))
		for _, p := range tb.Points {
			if p.Altitude != nil {
				coords = append(coords, []float64{p.Longitude, p.Latitude, *p.Altitude})
			} else {
				coords = append(coords, []float64{p.Longitude, p.Latitude})
			}
		}
		features = append(features, map[string]any{
			"type": "Feature",
			"properties": map[string]any{
				"kind":        "trail",
				"date":        tb.Date,
				"pointCount":  tb.PointCount,
				"distanceKm":  tb.DistanceKm,
				"coordSystem": tb.CoordSystem,
			},
			"geometry": map[string]any{
				"type":        "LineString",
				"coordinates": coords,
			},
		})
	}
	for _, s := range tb.Stays {
		coord := []float64{s.Longitude, s.Latitude}
		if s.Altitude != 0 {
			coord = append(coord, s.Altitude)
		}
		props := map[string]any{
			"kind":            "stayPoint",
			"index":           s.Index,
			"startTime":       s.StartTime,
			"endTime":         s.EndTime,
			"durationSeconds": s.DurationS,
			"stayRadius":      s.RadiusM,
		}
		if s.EventID != "" {
			props["sourceEventId"] = s.EventID
		}
		if s.EventTitle != "" {
			props["eventTitle"] = s.EventTitle
		}
		if s.Location != "" {
			props["location"] = s.Location
		}
		features = append(features, map[string]any{
			"type":       "Feature",
			"properties": props,
			"geometry": map[string]any{
				"type":        "Point",
				"coordinates": coord,
			},
		})
	}
	fc := map[string]any{
		"type":     "FeatureCollection",
		"features": features,
	}
	b, _ := json.MarshalIndent(fc, "", "  ")
	return string(b) + "\n"
}

func (tb *trailBundle) KML() string {
	var b strings.Builder
	loc := tb.loc()
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<kml xmlns="http://www.opengis.net/kml/2.2">` + "\n  <Document>\n")
	fmt.Fprintf(&b, "    <name>RayNeo Lifelog %s</name>\n", xmlEscape(tb.Date))
	for _, s := range tb.Stays {
		name := fmt.Sprintf("Stay #%d (%s)", s.Index, formatDurationHuman(s.DurationS))
		if s.EventTitle != "" {
			name = s.EventTitle
		}
		desc := fmt.Sprintf("%s-%s (%s)",
			time.UnixMilli(s.StartTime).In(loc).Format("15:04"),
			time.UnixMilli(s.EndTime).In(loc).Format("15:04"),
			formatDurationHuman(s.DurationS))
		if s.Location != "" {
			desc += " · " + s.Location
		}
		fmt.Fprintf(&b, "    <Placemark>\n      <name>%s</name>\n      <description>%s</description>\n      <Point><coordinates>%.7f,%.7f,%.2f</coordinates></Point>\n    </Placemark>\n",
			xmlEscape(name), xmlEscape(desc), s.Longitude, s.Latitude, s.Altitude)
	}
	if len(tb.Points) >= 2 {
		fmt.Fprintf(&b, "    <Placemark>\n      <name>Trail %s (%.2f km)</name>\n      <LineString>\n        <tessellate>1</tessellate>\n        <coordinates>\n",
			xmlEscape(tb.Date), tb.DistanceKm)
		for _, p := range tb.Points {
			alt := 0.0
			if p.Altitude != nil {
				alt = *p.Altitude
			}
			fmt.Fprintf(&b, "          %.7f,%.7f,%.2f\n", p.Longitude, p.Latitude, alt)
		}
		b.WriteString("        </coordinates>\n      </LineString>\n    </Placemark>\n")
	}
	b.WriteString("  </Document>\n</kml>\n")
	return b.String()
}

// ---------- Geo math helpers ----------

func haversineKm(lat1, lon1, lat2, lon2 float64) float64 {
	const r = 6371.0
	dLat := (lat2 - lat1) * math.Pi / 180
	dLon := (lon2 - lon1) * math.Pi / 180
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180)*math.Cos(lat2*math.Pi/180)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * r * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func outOfChina(lat, lon float64) bool {
	return lon < 72.004 || lon > 137.8347 || lat < 0.8293 || lat > 55.8271
}

func gcj02Delta(lat, lon float64) (dLat, dLon float64) {
	const (
		a  = 6378245.0
		ee = 0.00669342162296594323
	)
	x, y := lon-105.0, lat-35.0
	dl := -100.0 + 2.0*x + 3.0*y + 0.2*y*y + 0.1*x*y + 0.2*math.Sqrt(math.Abs(x))
	dl += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	dl += (20.0*math.Sin(y*math.Pi) + 40.0*math.Sin(y/3.0*math.Pi)) * 2.0 / 3.0
	dl += (160.0*math.Sin(y/12.0*math.Pi) + 320*math.Sin(y*math.Pi/30.0)) * 2.0 / 3.0

	dn := 300.0 + x + 2.0*y + 0.1*x*x + 0.1*x*y + 0.1*math.Sqrt(math.Abs(x))
	dn += (20.0*math.Sin(6.0*x*math.Pi) + 20.0*math.Sin(2.0*x*math.Pi)) * 2.0 / 3.0
	dn += (20.0*math.Sin(x*math.Pi) + 40.0*math.Sin(x/3.0*math.Pi)) * 2.0 / 3.0
	dn += (150.0*math.Sin(x/12.0*math.Pi) + 300.0*math.Sin(x/30.0*math.Pi)) * 2.0 / 3.0

	radLat := lat / 180.0 * math.Pi
	magic := math.Sin(radLat)
	magic = 1 - ee*magic*magic
	sqrtMagic := math.Sqrt(magic)
	dLat = (dl * 180.0) / ((a * (1 - ee)) / (magic * sqrtMagic) * math.Pi)
	dLon = (dn * 180.0) / (a / sqrtMagic * math.Cos(radLat) * math.Pi)
	return dLat, dLon
}

// gcj02ToWGS84 iteratively inverts the GCJ-02 offset to sub-meter accuracy.
func gcj02ToWGS84(lat, lon float64) (float64, float64) {
	if outOfChina(lat, lon) {
		return lat, lon
	}
	wLat, wLon := lat, lon
	for i := 0; i < 4; i++ {
		dLat, dLon := gcj02Delta(wLat, wLon)
		wLat = lat - dLat
		wLon = lon - dLon
	}
	return wLat, wLon
}
