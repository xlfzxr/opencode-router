package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode-router/internal/egress"
)

const (
	openCodeUA          = "opencode/latest/2.0.26/cli"
	defaultProject      = "global"
	defaultAnthropicVer = "2023-06-01"
	maxErrorBody        = 64 << 10
)

type Client struct {
	CatalogURL      string
	CatalogProvider string
	BaseURL         string
	Egress          *egress.Manager
	Sessions        *SessionStore
	RequestTimeout  time.Duration
	StreamIdle      time.Duration
	CatalogTTL      time.Duration

	catalogMu sync.Mutex
	catalogAt time.Time
	catalog   []Model
}

type CallOptions struct {
	Proxy      string
	Circuit    string
	SessionPin string
	Header     http.Header
	// Stream indicates the downstream client asked for SSE. Streaming calls get
	// no total deadline (only the per-read idle watchdog); JSON calls are bounded
	// by RequestTimeout.
	Stream bool
}

type Response struct {
	*http.Response
	cancel context.CancelFunc
}

func (r *Response) Close() error {
	err := r.Response.Body.Close()
	if r.cancel != nil {
		r.cancel()
	}
	return err
}

type UpstreamError struct {
	Status     int
	Kind       string
	Body       string
	RetryAfter time.Duration
}

func (e *UpstreamError) Error() string {
	if e.Kind != "" {
		return fmt.Sprintf("upstream %d (%s): %s", e.Status, e.Kind, truncate(e.Body, 300))
	}
	return fmt.Sprintf("upstream %d: %s", e.Status, truncate(e.Body, 300))
}

// Do prepares the request body for the target endpoint, resolves the stable
// session, selects an egress, and performs the call. Upstream is always asked
// to stream; callers aggregate when the downstream client asked for JSON.
func (c *Client) Do(ctx context.Context, kind Kind, model string, body map[string]any, opts CallOptions) (*Response, map[string]string, error) {
	rename := prepareBody(kind, model, body)
	session := c.Sessions.Resolve(body, opts.Header, opts.SessionPin, "")

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}

	sel, err := c.Egress.Select(c.BaseURL, opts.Proxy, opts.Circuit)
	if err != nil {
		return nil, nil, err
	}
	transport, err := c.Egress.Transport(sel)
	if err != nil {
		return nil, nil, err
	}

	call := func(sel egress.Selection) (*http.Response, context.CancelFunc, error) {
		reqCtx := ctx
		var cancel context.CancelFunc
		if !opts.Stream {
			reqCtx, cancel = context.WithTimeout(ctx, c.RequestTimeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.BaseURL+kind.Path(), bytes.NewReader(payload))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, err
		}
		setHeaders(req, session, kind, true)
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return nil, nil, err
		}
		return resp, cancel, nil
	}

	resp, cancel, err := call(sel)
	if err != nil && !sel.IsDirect() && !c.Egress.Strict() {
		// Non-strict proxy: fall back to direct once, mirroring 9router's
		// promise that a broken proxy does not take the router down.
		direct := egress.Selection{Source: egress.SourceDirect}
		if t, terr := c.Egress.Transport(direct); terr == nil {
			_ = t
			resp, cancel, err = call(direct)
		}
	}
	if err != nil {
		return nil, rename, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		if cancel != nil {
			cancel()
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, rename, parseUpstreamError(resp.StatusCode, data, resp.Header)
	}
	return &Response{Response: resp, cancel: cancel}, rename, nil
}

func prepareBody(kind Kind, model string, body map[string]any) map[string]string {
	if body == nil {
		body = map[string]any{}
	}
	body["model"] = model
	body["stream"] = true

	switch kind {
	case KindResponses:
		normalizeResponses(body)
		return ApplyFingerprintTools(body, true)
	case KindMessages:
		return ApplyAnthropicFingerprint(body)
	default:
		rename := ApplyFingerprintTools(body, false)
		injectReasoningContent(body)
		return rename
	}
}

// injectReasoningContent mirrors 9router's opencode provider rule (scope "all"):
// reasoning models reject multi-turn history whose assistant messages omit
// reasoning_content, so a non-empty placeholder is echoed back.
func injectReasoningContent(body map[string]any) {
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if existing, ok := msg["reasoning_content"].(string); ok && existing != "" {
			continue
		}
		msg["reasoning_content"] = " "
	}
}

func normalizeResponses(body map[string]any) {
	if _, ok := body["max_output_tokens"]; !ok {
		if v, ok := body["max_completion_tokens"]; ok {
			body["max_output_tokens"] = v
		} else if v, ok := body["max_tokens"]; ok {
			body["max_output_tokens"] = v
		}
	}
	delete(body, "max_tokens")
	delete(body, "max_completion_tokens")

	if effort, ok := body["reasoning_effort"].(string); ok && effort != "" {
		reasoning, _ := body["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		reasoning["effort"] = strings.ToLower(strings.TrimSpace(effort))
		body["reasoning"] = reasoning
		delete(body, "reasoning_effort")
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if _, ok := reasoning["summary"]; !ok {
			reasoning["summary"] = "auto"
		}
	}

	body["store"] = false
	if input, ok := body["input"].([]any); !ok || len(input) == 0 {
		body["input"] = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "..."}},
		}}
	}
	sanitizeResponsesInput(body)
}

func sanitizeResponsesInput(body map[string]any) {
	input, ok := body["input"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if item["type"] == "reasoning" {
			continue
		}
		delete(item, "encrypted_content")
		delete(item, "reasoning_encrypted_content")
		switch item["type"] {
		case "function_call":
			name, _ := item["name"].(string)
			if strings.TrimSpace(name) == "" {
				continue
			}
			item["name"] = truncate(strings.TrimSpace(name), 128)
			item["call_id"] = clampCallID(item["call_id"])
			item["arguments"] = coerceJSONString(item["arguments"], "{}")
		case "function_call_output":
			item["call_id"] = clampCallID(item["call_id"])
			item["output"] = coerceJSONString(item["output"], "")
		}
		out = append(out, item)
	}
	body["input"] = out
}

func clampCallID(v any) string {
	id := strings.TrimSpace(fmt.Sprint(v))
	var b strings.Builder
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := truncate(b.String(), 64)
	if out == "" {
		out = "call_" + randomBase62(16)
	}
	return out
}

func coerceJSONString(v any, empty string) string {
	switch value := v.(type) {
	case nil:
		return empty
	case string:
		return value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return empty
		}
		return string(data)
	}
}

func isEventStream(body map[string]any) bool {
	stream, _ := body["stream"].(bool)
	return stream
}

func setHeaders(req *http.Request, session string, kind Kind, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", openCodeUA)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("x-session-id", session)
	req.Header.Set("x-opencode-project", defaultProject)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "*/*")
	}
	if kind == KindMessages {
		req.Header.Set("anthropic-version", defaultAnthropicVer)
	}
}

func parseUpstreamError(status int, body []byte, header http.Header) *UpstreamError {
	out := &UpstreamError{Status: status, Body: string(body)}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		out.Kind = errorKind(payload)
		out.RetryAfter = retryAfter(payload, header)
	}
	if out.RetryAfter == 0 {
		if raw := header.Get("Retry-After"); raw != "" {
			if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
				out.RetryAfter = time.Duration(seconds) * time.Second
			}
		}
	}
	return out
}

func errorKind(payload map[string]any) string {
	if nested, ok := payload["error"].(map[string]any); ok {
		if kind, ok := nested["type"].(string); ok {
			return kind
		}
	}
	if kind, ok := payload["type"].(string); ok {
		return kind
	}
	if code, ok := payload["code"]; ok && fmt.Sprint(code) != "" {
		return fmt.Sprint(code)
	}
	return ""
}

func retryAfter(payload map[string]any, header http.Header) time.Duration {
	for _, key := range []string{"retry_after", "reset_after", "reset_after_ms"} {
		if v, ok := payload[key]; ok {
			if seconds, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil {
				if strings.HasSuffix(key, "_ms") {
					return time.Duration(seconds) * time.Millisecond
				}
				return time.Duration(seconds * float64(time.Second))
			}
		}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if v, ok := nested["retry_after"]; ok {
			if seconds, err := strconv.ParseFloat(fmt.Sprint(v), 64); err == nil {
				return time.Duration(seconds * float64(time.Second))
			}
		}
	}
	if raw := header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

func (c *Client) ListModels(ctx context.Context, opts CallOptions) ([]Model, error) {
	c.catalogMu.Lock()
	if len(c.catalog) > 0 && time.Since(c.catalogAt) < c.CatalogTTL {
		cached := append([]Model(nil), c.catalog...)
		c.catalogMu.Unlock()
		return cached, nil
	}
	c.catalogMu.Unlock()

	sel, err := c.Egress.Select(c.BaseURL, opts.Proxy, opts.Circuit)
	if err != nil {
		return nil, err
	}
	transport, err := c.Egress.Transport(sel)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.RequestTimeout)
	defer cancel()

	session := c.Sessions.Resolve(nil, opts.Header, opts.SessionPin, "")
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.BaseURL+"/inference/v1/models", nil)
	if err != nil {
		return nil, err
	}
	setHeaders(req, session, KindChat, false)

	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, parseUpstreamError(resp.StatusCode, data, resp.Header)
	}
	models := ParseCatalog(data)
	if len(models) == 0 {
		return nil, fmt.Errorf("empty upstream catalog")
	}
	metadata, err := c.modelsDev(ctx, opts)
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	for _, m := range models {
		live[m.ID] = true
	}
	filtered := []Model{}
	for _, m := range metadata {
		if live[m.ID] {
			filtered = append(filtered, m)
		}
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("no free models shared by models.dev and upstream catalog")
	}
	c.catalogMu.Lock()
	c.catalog = append([]Model(nil), filtered...)
	c.catalogAt = time.Now()
	c.catalogMu.Unlock()
	return filtered, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
