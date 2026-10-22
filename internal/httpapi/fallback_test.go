package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"opencode-router/internal/config"
	"opencode-router/internal/egress"
	"opencode-router/internal/routing"
	"opencode-router/internal/upstream"
)

// fakeUpstream emulates the opencode.ai inference endpoints: a catalog plus
// per-model chat behaviour, so the fallback loop can be tested deterministically.
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	sseHeaders := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/inference/v1/models":
			writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": []any{
				map[string]any{"id": "bad-free", "object": "model", "owned_by": "opencode"},
				map[string]any{"id": "badreq-free", "object": "model", "owned_by": "opencode"},
				map[string]any{"id": "empty-free", "object": "model", "owned_by": "opencode"},
				map[string]any{"id": "good-free", "object": "model", "owned_by": "opencode"},
			}})
		case "/inference/v1/chat/completions":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			model, _ := body["model"].(string)
			switch model {
			case "bad-free":
				writeJSON(w, http.StatusInternalServerError, map[string]any{
					"error": map[string]any{"type": "server_error", "message": "boom capacity"},
				})
			case "empty-free":
				sseHeaders(w)
				fmt.Fprint(w, "data: {\"id\":\"x\",\"model\":\"empty-free\",\"choices\":[{\"index\":0,\"delta\":{}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			case "badreq-free":
				writeJSON(w, http.StatusBadRequest, map[string]any{
					"error": map[string]any{"type": "invalid_request", "message": "context length exceeded"},
				})
			default:
				sseHeaders(w)
				fmt.Fprintf(w, "data: {\"id\":\"1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":null}]}\n\n", model)
				fmt.Fprintf(w, "data: {\"id\":\"1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1}}\n\n", model)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func testServer(t *testing.T, baseURL string, candidates []string) *httptest.Server {
	t.Helper()
	client := &upstream.Client{
		BaseURL:        baseURL,
		Egress:         egress.New(config.Proxy{}, 5*time.Second),
		Sessions:       upstream.NewSessionStore(time.Hour, 100),
		RequestTimeout: 10 * time.Second,
		StreamIdle:     10 * time.Second,
		CatalogTTL:     time.Minute,
	}
	router := routing.New(routing.Config{
		Enabled:     true,
		Alias:       "ocr",
		Strategy:    "fallback",
		AutoSwitch:  false,
		MaxAttempts: 3,
		Candidates:  candidates,
	})
	return httptest.NewServer(New(client, nil, router).Handler())
}

func postChat(t *testing.T, url, model string) (*http.Response, map[string]any) {
	t.Helper()
	payload := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":8}`, model)
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp, out
}

func TestFallbackOnUpstreamError(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	srv := testServer(t, up.URL, []string{"bad-free", "good-free"})
	defer srv.Close()

	resp, out := postChat(t, srv.URL, "ocr")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d: %v", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-OCR-Model"); got != "good-free" {
		t.Fatalf("X-OCR-Model = %q, want good-free", got)
	}
	if got := resp.Header.Get("X-OCR-Attempt"); got != "2" {
		t.Fatalf("X-OCR-Attempt = %q, want 2", got)
	}
	if model, _ := out["model"].(string); model != "good-free" {
		t.Fatalf("body model = %q, want good-free", model)
	}
}

func TestFallbackOnZeroContent(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	srv := testServer(t, up.URL, []string{"empty-free", "good-free"})
	defer srv.Close()

	resp, out := postChat(t, srv.URL, "ocr")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after fallback, got %d: %v", resp.StatusCode, out)
	}
	if got := resp.Header.Get("X-OCR-Model"); got != "good-free" {
		t.Fatalf("X-OCR-Model = %q, want good-free", got)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("no choices in %v", out)
	}
}

func TestNoFallbackOnRequestScopedError(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	srv := testServer(t, up.URL, []string{"badreq-free", "good-free"})
	defer srv.Close()

	resp, _ := postChat(t, srv.URL, "ocr")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 (no fallback), got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("X-OCR-Attempt"); got != "1" {
		t.Fatalf("X-OCR-Attempt = %q, want 1", got)
	}
}

func TestCooldownSkipsFailedModelNextRequest(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	srv := testServer(t, up.URL, []string{"bad-free", "good-free"})
	defer srv.Close()

	// First request: bad-free fails, good-free serves.
	postChat(t, srv.URL, "ocr")

	// Second request: bad-free is cooling down, so it should be demoted.
	resp, _ := postChat(t, srv.URL, "ocr")
	if got := resp.Header.Get("X-OCR-Model"); got != "good-free" {
		t.Fatalf("X-OCR-Model = %q, want good-free", got)
	}
	if got := resp.Header.Get("X-OCR-Attempt"); got != "1" {
		t.Fatalf("X-OCR-Attempt = %q, want 1 (bad-free cooled down)", got)
	}
}

func TestAliasRejectedOnResponsesEndpoint(t *testing.T) {
	up := fakeUpstream(t)
	defer up.Close()
	srv := testServer(t, up.URL, []string{"good-free"})
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"ocr","input":"hi"}`))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for alias on /v1/responses, got %d", resp.StatusCode)
	}
}
