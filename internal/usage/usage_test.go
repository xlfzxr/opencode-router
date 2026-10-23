package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStatsAggregation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "usage.json"), true, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()

	now := time.Now()
	store.insert(Entry{
		Time: now.Add(-3 * time.Minute), Model: "big-pickle", Endpoint: "chat",
		Status: "ok", HTTPStatus: 200, LatencyMs: 100,
		PromptTokens: 50, CompletionTokens: 5, Stream: false, Proxy: "default",
	})
	store.insert(Entry{
		Time: now.Add(-2 * time.Minute), Model: "glm-5.3-flash-free", Endpoint: "chat",
		Status: "error", HTTPStatus: 502, LatencyMs: 80, Proxy: "tor",
		Error: "upstream 502",
	})
	store.insert(Entry{
		Time: now.Add(-time.Minute), Model: "glm-5.3-flash-free", Endpoint: "chat",
		Status: "ok", HTTPStatus: 200, LatencyMs: 120,
		PromptTokens: 100, CompletionTokens: 20, Stream: true, Proxy: "tor",
	})

	stats, err := store.Stats("today", now)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Totals.Requests != 3 || stats.Totals.OK != 2 || stats.Totals.Errors != 1 {
		t.Fatalf("unexpected totals: %+v", stats.Totals)
	}
	if stats.Totals.PromptTokens != 150 || stats.Totals.CompletionTokens != 25 {
		t.Fatalf("unexpected tokens: %+v", stats.Totals)
	}
	if stats.Totals.TotalTokens != 175 {
		t.Fatalf("total tokens = %d, want 175", stats.Totals.TotalTokens)
	}
	if stats.Totals.Streams != 1 {
		t.Fatalf("streams = %d, want 1", stats.Totals.Streams)
	}
	if len(stats.ByModel) != 2 {
		t.Fatalf("byModel = %d entries, want 2", len(stats.ByModel))
	}
	if stats.ByModel[0].Model != "glm-5.3-flash-free" || stats.ByModel[0].TotalTokens != 120 {
		t.Fatalf("byModel order wrong: %+v", stats.ByModel)
	}
	if stats.ByModel[0].Errors != 1 {
		t.Fatalf("glm errors = %d, want 1", stats.ByModel[0].Errors)
	}
	if len(stats.Recent) != 3 {
		t.Fatalf("recent = %d, want 3", len(stats.Recent))
	}
	if stats.Recent[0].Model != "glm-5.3-flash-free" || !stats.Recent[0].Stream {
		t.Fatalf("recent[0] = %+v", stats.Recent[0])
	}
	if len(stats.Series) == 0 {
		t.Fatal("expected a non-empty series")
	}
}

func TestInvalidPeriod(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "usage.json"), true, 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer store.Close()
	if _, err := store.Stats("yesterday", time.Now()); err == nil {
		t.Fatal("expected an error for an invalid period")
	}
}

func TestDisabledStore(t *testing.T) {
	store := Disabled()
	store.Record(Entry{Model: "x", Status: "ok"})
	stats, err := store.Stats("all", time.Now())
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Totals.Requests != 0 {
		t.Fatalf("requests = %d, want 0", stats.Totals.Requests)
	}
	if len(stats.Series) != 0 {
		t.Fatalf("series = %d, want empty", len(stats.Series))
	}
}

func TestFillSeriesContinuous(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, time.Local)
	start := time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)
	points := map[string]SeriesPoint{
		"2026-10-22 13:00": {Label: "2026-10-22 13:00", Requests: 2},
	}
	series := fillSeries(points, start, now, "hour")
	if len(series) != 4 {
		t.Fatalf("series length = %d, want 4", len(series))
	}
	if series[1].Requests != 2 {
		t.Fatalf("13:00 requests = %d, want 2", series[1].Requests)
	}
	if series[0].Requests != 0 || series[3].Requests != 0 {
		t.Fatal("expected zero-filled gaps")
	}
}

func TestJSONPersistenceAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if other, err := Open(path, true, 0); err == nil {
		other.Close()
		t.Fatal("expected exclusive lock")
	}
	s.Record(Entry{Model: "saved", PromptTokens: 3, CompletionTokens: 4})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Close is idempotent, and a late Record cannot panic.
	s.Record(Entry{Model: "too late"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !json.Valid(raw) {
		t.Fatalf("invalid snapshot: %s, %v", raw, err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %v", info.Mode())
	}
	reopened, err := Open(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stats, err := reopened.Stats("all", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Totals.Requests != 1 || stats.Totals.TotalTokens != 7 || stats.Recent[0].Status != "ok" {
		t.Fatalf("lost data: %+v", stats)
	}
	reopened.insert(Entry{Model: "next"})
	stats, _ = reopened.Stats("all", time.Now())
	if stats.Recent[0].ID != 2 {
		t.Fatalf("ID not preserved: %+v", stats.Recent)
	}
}

func TestRejectInvalidFilesWithoutOverwriting(t *testing.T) {
	for _, content := range []string{"SQLite format 3\x00old data", "{broken", "", `{"version":2,"nextId":1}`, `{"version":1,"nextId":1,"entries":[{"id":1}]}`} {
		path := filepath.Join(t.TempDir(), "old.db")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path, true, 0); err == nil {
			s.Close()
			t.Fatalf("accepted %q", content)
		}
		raw, _ := os.ReadFile(path)
		if string(raw) != content {
			t.Fatal("existing file modified")
		}
		// A failed open must release its lock.
		os.Remove(path)
		s, err := Open(path, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestRetentionAndRecentLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	s.insert(Entry{Time: time.Now().AddDate(0, 0, -10), Model: "expired"})
	for i := 0; i < 60; i++ {
		s.Record(Entry{Model: "recent"})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stats, _ := s.Stats("all", time.Now())
	if stats.Totals.Requests != 60 || len(stats.Recent) != 50 {
		t.Fatalf("retention/recent: %+v", stats)
	}
	if stats.Recent[0].ID != 61 {
		t.Fatalf("next ID changed: %+v", stats.Recent[0])
	}
}

func TestConcurrentRecordStatsClose(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.json"), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.Record(Entry{Model: "parallel"})
				s.Stats("24h", time.Now())
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); s.Close() }()
	wg.Wait()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodTimezoneAndTotals(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "usage.json"), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	zone := time.FixedZone("Jakarta", 7*3600)
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, zone)
	s.insert(Entry{Time: now.Add(-time.Hour).UTC(), Model: "today", LatencyMs: 100})
	s.insert(Entry{Time: now.AddDate(0, 0, -2), Model: "older", LatencyMs: 200})
	stats, _ := s.Stats("today", now)
	if stats.Totals.Requests != 1 || stats.Totals.AvgLatencyMs != 100 || len(stats.Recent) != 2 {
		t.Fatalf("period filter: %+v", stats)
	}
	found := false
	for _, p := range stats.Series {
		if p.Label == "2026-10-23 11:00" && p.Requests == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("timezone bucket: %+v", stats.Series)
	}
	for _, period := range []string{"24h", "7d", "30d", "60d", "all"} {
		stats, err := s.Stats(period, now)
		if err != nil {
			t.Fatal(err)
		}
		want := 2
		if period == "24h" {
			want = 1
		}
		if stats.Totals.Requests != want {
			t.Fatalf("%s: %+v", period, stats.Totals)
		}
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("OCR_USAGE_PATH", "")
	t.Setenv("OCR_USAGE_DB", "")
	t.Setenv("HOME", t.TempDir())
	if filepath.Base(DefaultPath()) != "usage.json" {
		t.Fatal(DefaultPath())
	}
	t.Setenv("OCR_USAGE_DB", "legacy-override.json")
	if DefaultPath() != "legacy-override.json" {
		t.Fatal(DefaultPath())
	}
	t.Setenv("OCR_USAGE_PATH", "preferred.json")
	if DefaultPath() != "preferred.json" {
		t.Fatal(DefaultPath())
	}
}

func TestFailedSaveKeepsOldSnapshotAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	s, err := Open(path, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original, _ := os.ReadFile(path)
	s.insert(Entry{Model: "retry"})
	// Redirect writes to a missing parent to simulate a storage failure.
	s.mu.Lock()
	s.path = filepath.Join(filepath.Dir(path), "missing", "usage.json")
	s.mu.Unlock()
	if err := s.persist(); err == nil {
		t.Fatal("expected write failure")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(original) {
		t.Fatal("failed save changed previous snapshot")
	}
	s.mu.RLock()
	dirty := s.dirty
	s.mu.RUnlock()
	if !dirty {
		t.Fatal("failed save must remain pending")
	}
	s.mu.Lock()
	s.path = path
	s.mu.Unlock()
	if err := s.persist(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	var saved snapshot
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.Entries) != 1 || saved.Entries[0].Model != "retry" {
		t.Fatalf("retry lost data: %+v", saved)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".usage-*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files leaked: %v", leftovers)
	}
}
