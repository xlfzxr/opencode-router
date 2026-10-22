package usage

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStatsAggregation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "usage.db"), true, 0)
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
	store, err := Open(filepath.Join(t.TempDir(), "usage.db"), true, 0)
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
