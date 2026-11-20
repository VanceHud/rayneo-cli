package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIsCacheable(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   bool
	}{
		{"GET", "/lifelog/api/v1/events", true},
		{"GET", "/action/api/v2/tasks", true},
		{"GET", "/profileapi/geo/reverseGeocode", true},
		{"GET", "/identify/device/list", false},
		{"POST", "/lifelog/api/v1/calendar", true},
		{"POST", "/lifelog/api/v1/journal/detail", true},
		{"POST", "/clsapi/aiTask/getDataAllByTaskId", true},
		{"POST", "/action/api/v2/tasks/create", false},
		{"POST", "/action/api/v2/tasks/123", false},
		{"POST", "/action/api/v2/tasks/123/complete", false},
		{"POST", "/action/api/v2/tasks/123/reopen", false},
		{"DELETE", "/action/api/v2/tasks/123", false},
		{"PUT", "/action/api/v2/tasks/123", false},
	}
	for _, tc := range cases {
		if got := IsCacheable(tc.method, tc.path); got != tc.want {
			t.Errorf("IsCacheable(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestAutoTTLFor(t *testing.T) {
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	today := now.Format("2006-01-02")
	tomorrow := now.AddDate(0, 0, 1).Format("2006-01-02")

	if got := AutoTTLFor("/lifelog/api/v1/events", "date="+yesterday, ""); got != TTLPast {
		t.Errorf("past date TTL = %v, want %v", got, TTLPast)
	}
	if got := AutoTTLFor("/lifelog/api/v1/events", "date="+today, ""); got != TTLToday {
		t.Errorf("today TTL = %v, want %v", got, TTLToday)
	}
	if got := AutoTTLFor("/lifelog/api/v1/events", "date="+tomorrow, ""); got != TTLToday {
		t.Errorf("future date TTL = %v, want %v", got, TTLToday)
	}
	if got := AutoTTLFor("/lifelog/api/v1/events/123456/full", "", ""); got != TTLEventDetail {
		t.Errorf("event full TTL = %v, want %v", got, TTLEventDetail)
	}
	if got := AutoTTLFor("/clsapi/aiTask/getDataAllByTaskId", "", `{"taskIds":["abc"]}`); got != TTLEventDetail {
		t.Errorf("aiTask transcript TTL = %v, want %v", got, TTLEventDetail)
	}
	if got := AutoTTLFor("/profileapi/geo/reverseGeocode", "latitude=36&longitude=120", ""); got != TTLPast {
		t.Errorf("reverseGeocode TTL = %v, want %v", got, TTLPast)
	}
	if got := AutoTTLFor("/action/api/v2/tasks", "accountId=1", ""); got != TTLVolatile {
		t.Errorf("tasks list TTL = %v, want %v", got, TTLVolatile)
	}
}

func TestCachePutGetInvalidateAndPrune(t *testing.T) {
	dir := t.TempDir()
	c := New(dir)

	// Write request must never be cached.
	c.Put("POST", "/action/api/v2/tasks/create", "", []byte(`{"content":"x"}`), []byte(`{"code":0}`), time.Hour)
	if _, ok := c.Get("POST", "/action/api/v2/tasks/create", "", []byte(`{"content":"x"}`)); ok {
		t.Fatal("POST create should not be cached")
	}

	// Read request should cache and invalidate by prefix.
	c.Put("GET", "/action/api/v2/tasks", "sources=rayneoTasks", nil, []byte(`{"code":0,"data":{"pending":[]}}`), time.Hour)
	c.Put("GET", "/lifelog/api/v1/count", "", nil, []byte(`{"code":0}`), time.Hour)
	if _, ok := c.Get("GET", "/action/api/v2/tasks", "sources=rayneoTasks", nil); !ok {
		t.Fatal("expected tasks GET cache hit")
	}
	removed := c.InvalidatePrefix("/action/api/v2/tasks")
	if removed != 1 {
		t.Fatalf("InvalidatePrefix removed=%d, want 1", removed)
	}
	if _, ok := c.Get("GET", "/action/api/v2/tasks", "sources=rayneoTasks", nil); ok {
		t.Fatal("expected tasks cache to be invalidated")
	}
	if _, ok := c.Get("GET", "/lifelog/api/v1/count", "", nil); !ok {
		t.Fatal("unrelated count cache should remain")
	}

	// Add an already-expired entry and test Prune.
	stalePath := filepath.Join(dir, "ff", "stale.json")
	if err := os.MkdirAll(filepath.Dir(stalePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stalePath, []byte(`{"fetched_at":1,"ttl_seconds":60,"path":"/test","body":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	pruned, _ := c.Prune()
	if pruned != 1 {
		t.Fatalf("Prune removed=%d, want 1", pruned)
	}
	if _, ok := c.Get("GET", "/lifelog/api/v1/count", "", nil); !ok {
		t.Fatal("fresh count cache should survive Prune")
	}
}
