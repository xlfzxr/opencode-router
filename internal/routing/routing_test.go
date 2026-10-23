package routing

import (
	"testing"
	"time"
)

func TestDetectRequiredCapabilities(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": "previous answer"},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "what is this?"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			}},
		},
	}
	req := detectRequiredCapabilities(body)
	if !req["vision"] {
		t.Fatalf("expected vision capability, got %v", req)
	}

	// Historical (non-trailing) images must not count.
	body = map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			}},
			map[string]any{"role": "assistant", "content": "seen"},
			map[string]any{"role": "user", "content": "just text now"},
		},
	}
	if req := detectRequiredCapabilities(body); len(req) != 0 {
		t.Fatalf("expected no required capabilities, got %v", req)
	}

	// Data-URI PDF in string content.
	body = map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "check data:application/pdf;base64,AAAA"},
		},
	}
	if req := detectRequiredCapabilities(body); !req["pdf"] {
		t.Fatalf("expected pdf capability, got %v", req)
	}

	// Responses-shape input with an image block.
	body = map[string]any{
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{
				map[string]any{"type": "input_image", "image_url": "https://example.com/b.png"},
			}},
		},
	}
	if req := detectRequiredCapabilities(body); !req["vision"] {
		t.Fatalf("expected vision capability from input, got %v", req)
	}
}

func TestReorderByCapabilities(t *testing.T) {
	models := []string{"flint-40b-free", "big-pickle", "nemotron-3.5-super-vl-free", "glm-5.3-flash-free"}
	out := reorderByCapabilities(models, map[string]bool{"vision": true})
	if len(out) != len(models) {
		t.Fatalf("model dropped: %v", out)
	}
	// Vision-capable models first (stable: super-vl before glm), then the rest
	// in their original relative order.
	want := []string{"nemotron-3.5-super-vl-free", "glm-5.3-flash-free", "flint-40b-free", "big-pickle"}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("order mismatch at %d: got %v want %v", i, out, want)
		}
	}
}

func TestPlanFallbackOrder(t *testing.T) {
	r := New(Config{
		Enabled:     true,
		Alias:       "ocr",
		Strategy:    "fallback",
		AutoSwitch:  false,
		MaxAttempts: 3,
		Candidates:  []string{"a-free", "b-free", "c-free"},
	})
	plan := r.Plan(map[string]any{}, nil)
	want := []string{"a-free", "b-free", "c-free"}
	for i := range want {
		if plan[i] != want[i] {
			t.Fatalf("plan mismatch: got %v want %v", plan, want)
		}
	}
}

func TestPlanAvailabilityFilter(t *testing.T) {
	r := New(Config{
		Enabled:    true,
		AutoSwitch: false,
		Candidates: []string{"a-free", "b-free", "c-free"},
	})
	plan := r.Plan(map[string]any{}, []string{"b-free", "c-free"})
	if len(plan) != 2 || plan[0] != "b-free" || plan[1] != "c-free" {
		t.Fatalf("availability filter wrong: %v", plan)
	}

	// No intersection: never send requests to models absent from the catalog.
	plan = r.Plan(map[string]any{}, []string{"zzz"})
	if len(plan) != 0 {
		t.Fatalf("expected empty pool, got %v", plan)
	}
}

func TestRoundRobinSticky(t *testing.T) {
	r := New(Config{
		Enabled:     true,
		Strategy:    "round-robin",
		StickyLimit: 2,
		AutoSwitch:  false,
		Candidates:  []string{"a", "b", "c"},
	})
	first := func() string { return r.Plan(map[string]any{}, nil)[0] }
	if got := []string{first(), first(), first(), first(), first()}; got[0] != "a" || got[1] != "a" || got[2] != "b" || got[3] != "b" || got[4] != "c" {
		t.Fatalf("sticky rotation wrong: %v", got)
	}
}

func TestCooldownReordersPlan(t *testing.T) {
	r := New(Config{
		Enabled:    true,
		AutoSwitch: false,
		Candidates: []string{"a", "b"},
	})
	r.MarkFailed("a", 500, "internal error", 0)
	plan := r.Plan(map[string]any{}, nil)
	if plan[0] != "b" || plan[1] != "a" {
		t.Fatalf("cooldown should demote a: %v", plan)
	}
	r.MarkOK("a")
	plan = r.Plan(map[string]any{}, nil)
	if plan[0] != "a" {
		t.Fatalf("MarkOK should restore a: %v", plan)
	}
}

func TestShouldFallback(t *testing.T) {
	cases := []struct {
		status int
		text   string
		want   bool
	}{
		{400, "bad request", false},
		{400, "context length exceeded", false},
		{401, "", true},
		{403, "quota", true},
		{404, "model not found", true},
		{429, "", true},
		{500, "", true},
		{503, "", true},
		{0, "connection refused", true},
		{400, "rate limit exceeded", true},
		{502, "capacity reached", true},
	}
	for _, c := range cases {
		if got := ShouldFallback(c.status, c.text); got != c.want {
			t.Fatalf("ShouldFallback(%d, %q) = %v, want %v", c.status, c.text, got, c.want)
		}
	}
}

func TestAliasHelpers(t *testing.T) {
	r := New(Config{Enabled: true})
	if !r.IsAlias("ocr") || !r.IsAlias("ocr/ocr") || r.IsAlias("ocr/glm-5.3-flash-free") {
		t.Fatalf("IsAlias wrong")
	}
	if got := r.StripAlias("ocr/glm-5.3-flash-free"); got != "glm-5.3-flash-free" {
		t.Fatalf("StripAlias = %q", got)
	}
}

func TestZeroContentChat(t *testing.T) {
	// Mirrors 9router's isZeroContentResponse: only abnormal finishes with no
	// content/tools/reasoning count; an empty stop is passed through.
	abnormal := map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": ""},
	}}}
	if !ZeroContentChat(abnormal) {
		t.Fatal("missing finish_reason with empty content should be zero-content")
	}
	inProgress := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "in_progress",
		"message":       map[string]any{"role": "assistant", "content": ""},
	}}}
	if !ZeroContentChat(inProgress) {
		t.Fatal("in_progress with empty content should be zero-content")
	}
	emptyStop := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop",
		"message":       map[string]any{"role": "assistant", "content": ""},
	}}}
	if ZeroContentChat(emptyStop) {
		t.Fatal("empty stop is passed through, per 9router semantics")
	}
	ok := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "stop",
		"message":       map[string]any{"role": "assistant", "content": "hi"},
	}}}
	if ZeroContentChat(ok) {
		t.Fatal("content should not be zero-content")
	}
	withTools := map[string]any{"choices": []any{map[string]any{
		"finish_reason": "tool_calls",
		"message": map[string]any{"role": "assistant", "content": "",
			"tool_calls": []any{map[string]any{"id": "1"}}},
	}}}
	if ZeroContentChat(withTools) {
		t.Fatal("tool calls should not be zero-content")
	}
	reasoning := map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"role": "assistant", "content": "", "reasoning_content": "thinking..."},
	}}}
	if ZeroContentChat(reasoning) {
		t.Fatal("reasoning content should not be zero-content")
	}
}

func TestRateLimitBackoffGrows(t *testing.T) {
	r := New(Config{Enabled: true, AutoSwitch: false, Candidates: []string{"a"}})
	r.MarkFailed("a", 429, "", 0)
	r.mu.Lock()
	first := time.Until(r.cooldown["a"])
	r.mu.Unlock()
	r.MarkFailed("a", 429, "", 0)
	r.mu.Lock()
	second := time.Until(r.cooldown["a"])
	r.mu.Unlock()
	if first < 1900*time.Millisecond || first > 2100*time.Millisecond {
		t.Fatalf("first backoff = %v, want ~2s", first)
	}
	if second < 3900*time.Millisecond || second > 4100*time.Millisecond {
		t.Fatalf("second backoff = %v, want ~4s", second)
	}
}

func TestDynamicCandidates(t *testing.T) {
	r := New(Config{Enabled: true})
	plan := r.Plan(map[string]any{}, []string{"brand-new-free", "another-free"})
	if len(plan) != 2 || plan[0] != "brand-new-free" || plan[1] != "another-free" {
		t.Fatalf("did not discover new models: %v", plan)
	}
	if empty := r.Plan(map[string]any{}, nil); len(empty) != 0 {
		t.Fatalf("unexpected hardcoded fallback: %v", empty)
	}
}
