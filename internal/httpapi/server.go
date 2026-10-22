package httpapi

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"opencode-router/internal/upstream"
	"opencode-router/internal/usage"
)

const maxBodyBytes = 16 << 20

//go:embed dashboard.html
var dashboardHTML []byte

type Server struct {
	Client *upstream.Client
	Usage  *usage.Store
}

func New(client *upstream.Client, store *usage.Store) *Server {
	if store == nil {
		store = usage.Disabled()
	}
	return &Server{Client: client, Usage: store}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/usage/stats", s.handleUsageStats)
	mux.HandleFunc("/dashboard", s.handleDashboard)
	mux.HandleFunc("/dashboard/usage", s.handleDashboard)
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusFound)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleUsageStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	period := r.URL.Query().Get("period")
	stats, err := s.Usage.Stats(period, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	models, err := s.Client.ListModels(r.Context(), s.callOptions(r, false))
	if err != nil {
		w.Header().Set("X-OCR-Catalog", "fallback")
		log.Printf("[models] upstream catalog failed: %v", err)
	}
	data := make([]any, 0, len(models))
	for _, model := range models {
		data = append(data, map[string]any{
			"id":       model.ID,
			"object":   "model",
			"owned_by": model.OwnedBy,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	stream, _ := body["stream"].(bool)
	kind := upstream.KindFor(model)

	rec := s.beginRecord(r, "chat", model, stream)
	sw := &statusWriter{ResponseWriter: w}
	defer rec.commit(sw)

	switch kind {
	case upstream.KindChat:
		s.proxyChat(sw, r, body, model, stream, rec)
	case upstream.KindResponses:
		s.proxyChatToResponses(sw, r, body, model, stream, rec)
	case upstream.KindMessages:
		s.proxyChatToMessages(sw, r, body, model, stream, rec)
	}
}

func (s *Server) proxyChat(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool, rec *record) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindChat, model, body, s.callOptions(r, stream))
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	defer resp.Close()

	if stream {
		writeSSEHeaders(w)
		flush := flusher(w)
		flush()
		idle := upstream.NewIdleReader(resp.Body, s.Client.StreamIdle, cancel)
		defer idle.Stop()
		if err := upstream.StreamPassthrough(idle, w, flush, rename, rec.setUsage); err != nil {
			rec.fail(err)
			if idle.TimedOut() {
				log.Printf("[chat] stream stalled for %s; cancelled", s.Client.StreamIdle)
			} else {
				log.Printf("[chat] stream error: %v", err)
			}
		}
		return
	}

	out, err := upstream.AggregateChatSSE(resp.Body, model)
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	rec.setUsageFrom(out["usage"])
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) proxyChatToResponses(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool, rec *record) {
	body["model"] = model
	converted := upstream.ChatToResponses(body)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindResponses, model, converted, s.callOptions(r, stream))
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	defer resp.Close()

	if stream {
		writeSSEHeaders(w)
		flush := flusher(w)
		flush()
		idle := upstream.NewIdleReader(resp.Body, s.Client.StreamIdle, cancel)
		defer idle.Stop()
		if err := upstream.StreamResponsesToChat(idle, w, flush, model, rename, rec.setUsage); err != nil {
			rec.fail(err)
			if !idle.TimedOut() {
				log.Printf("[responses->chat] stream error: %v", err)
			}
		}
		return
	}
	out, err := upstream.AggregateResponsesSSE(resp.Body, model)
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	chat := upstream.ResponsesToChat(out, model)
	rec.setUsageFrom(chat["usage"])
	writeJSON(w, http.StatusOK, chat)
}

func (s *Server) proxyChatToMessages(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool, rec *record) {
	body["model"] = model
	converted := upstream.ChatToAnthropic(body)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindMessages, model, converted, s.callOptions(r, stream))
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	defer resp.Close()

	if stream {
		writeSSEHeaders(w)
		flush := flusher(w)
		flush()
		idle := upstream.NewIdleReader(resp.Body, s.Client.StreamIdle, cancel)
		defer idle.Stop()
		if err := upstream.StreamAnthropicToChat(idle, w, flush, model, rename, rec.setUsage); err != nil {
			rec.fail(err)
			if !idle.TimedOut() {
				log.Printf("[messages->chat] stream error: %v", err)
			}
		}
		return
	}
	out, err := upstream.AggregateAnthropicSSE(resp.Body, model)
	if err != nil {
		rec.fail(err)
		writeUpstreamError(w, err)
		return
	}
	chat := upstream.AnthropicToChat(out, model)
	rec.setUsageFrom(chat["usage"])
	writeJSON(w, http.StatusOK, chat)
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if kind := upstream.KindFor(model); kind != upstream.KindResponses {
		writeError(w, http.StatusBadRequest, "model "+model+" is not served by the responses endpoint; use /v1/chat/completions", "invalid_request_error")
		return
	}
	stream, _ := body["stream"].(bool)

	rec := s.beginRecord(r, "responses", model, stream)
	sw := &statusWriter{ResponseWriter: w}
	defer rec.commit(sw)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindResponses, model, body, s.callOptions(r, stream))
	if err != nil {
		rec.fail(err)
		writeUpstreamError(sw, err)
		return
	}
	defer resp.Close()

	if stream {
		writeSSEHeaders(sw)
		flush := flusher(sw)
		flush()
		idle := upstream.NewIdleReader(resp.Body, s.Client.StreamIdle, cancel)
		defer idle.Stop()
		if err := upstream.StreamPassthrough(idle, sw, flush, rename, rec.setUsage); err != nil {
			rec.fail(err)
			if !idle.TimedOut() {
				log.Printf("[responses] stream error: %v", err)
			}
		}
		return
	}
	out, err := upstream.AggregateResponsesSSE(resp.Body, model)
	if err != nil {
		rec.fail(err)
		writeUpstreamError(sw, err)
		return
	}
	rec.setUsageFrom(out["usage"])
	writeJSON(sw, http.StatusOK, out)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	model, _ := body["model"].(string)
	if model == "" {
		writeError(w, http.StatusBadRequest, "model is required", "invalid_request_error")
		return
	}
	if kind := upstream.KindFor(model); kind != upstream.KindMessages {
		writeError(w, http.StatusBadRequest, "model "+model+" is not served by the messages endpoint; use /v1/chat/completions", "invalid_request_error")
		return
	}
	stream, _ := body["stream"].(bool)

	rec := s.beginRecord(r, "messages", model, stream)
	sw := &statusWriter{ResponseWriter: w}
	defer rec.commit(sw)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindMessages, model, body, s.callOptions(r, stream))
	if err != nil {
		rec.fail(err)
		writeUpstreamError(sw, err)
		return
	}
	defer resp.Close()

	if stream {
		writeSSEHeaders(sw)
		flush := flusher(sw)
		flush()
		idle := upstream.NewIdleReader(resp.Body, s.Client.StreamIdle, cancel)
		defer idle.Stop()
		if err := upstream.StreamPassthrough(idle, sw, flush, rename, rec.setUsage); err != nil {
			rec.fail(err)
			if !idle.TimedOut() {
				log.Printf("[messages] stream error: %v", err)
			}
		}
		return
	}
	out, err := upstream.AggregateAnthropicSSE(resp.Body, model)
	if err != nil {
		rec.fail(err)
		writeUpstreamError(sw, err)
		return
	}
	rec.setUsageFrom(out["usage"])
	writeJSON(sw, http.StatusOK, out)
}

func (s *Server) callOptions(r *http.Request, stream bool) upstream.CallOptions {
	return upstream.CallOptions{
		Proxy:      r.Header.Get("X-OCR-Proxy"),
		Circuit:    r.Header.Get("X-OCR-Circuit"),
		SessionPin: r.Header.Get("X-OCR-Session"),
		Header:     r.Header,
		Stream:     stream,
	}
}

func decodeBody(r *http.Request) (map[string]any, error) {
	defer r.Body.Close()
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("empty request body")
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	return body, nil
}

func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func flusher(w http.ResponseWriter) func() {
	f, ok := w.(http.Flusher)
	if !ok {
		return func() {}
	}
	return f.Flush
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message, kind string) {
	if kind == "" {
		kind = "invalid_request_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
		},
	})
}

func writeUpstreamError(w http.ResponseWriter, err error) {
	var ue *upstream.UpstreamError
	if errors.As(err, &ue) {
		if ue.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(ue.RetryAfter.Seconds())))
		}
		status := ue.Status
		if ue.Kind == "FreeUsageLimitError" {
			status = http.StatusTooManyRequests
		}
		writeError(w, status, ue.Error(), ue.Kind)
		return
	}
	writeError(w, http.StatusBadGateway, err.Error(), "upstream_error")
}

// statusWriter remembers the status code while staying transparent for SSE
// flushing and ResponseController-based unwrapping.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type record struct {
	store      *usage.Store
	ctx        context.Context
	start      time.Time
	model      string
	endpoint   string
	proxy      string
	stream     bool
	prompt     int
	completion int
	failure    error
}

func (s *Server) beginRecord(r *http.Request, endpoint, model string, stream bool) *record {
	return &record{
		store:    s.Usage,
		ctx:      r.Context(),
		start:    time.Now(),
		model:    model,
		endpoint: endpoint,
		proxy:    sanitizeProxy(r.Header.Get("X-OCR-Proxy")),
		stream:   stream,
	}
}

func (rec *record) setUsage(prompt, completion int) {
	if prompt > 0 || completion > 0 {
		rec.prompt, rec.completion = prompt, completion
	}
}

func (rec *record) setUsageFrom(raw any) {
	u, ok := raw.(map[string]any)
	if !ok {
		return
	}
	prompt := int(numberOf(u["prompt_tokens"]))
	completion := int(numberOf(u["completion_tokens"]))
	if prompt == 0 {
		prompt = int(numberOf(u["input_tokens"]))
	}
	if completion == 0 {
		completion = int(numberOf(u["output_tokens"]))
	}
	rec.setUsage(prompt, completion)
}

func (rec *record) fail(err error) {
	if err != nil && rec.failure == nil {
		rec.failure = err
	}
}

func (rec *record) commit(sw *statusWriter) {
	if rec == nil || rec.store == nil {
		return
	}
	status := "ok"
	if rec.failure != nil || sw.status >= 400 {
		status = "error"
	}
	if rec.ctx != nil && errors.Is(rec.ctx.Err(), context.Canceled) {
		status = "canceled"
	}
	httpStatus := sw.status
	if httpStatus == 0 {
		httpStatus = http.StatusOK
	}
	errText := ""
	if rec.failure != nil {
		errText = rec.failure.Error()
	}
	rec.store.Record(usage.Entry{
		Time:             rec.start,
		Model:            rec.model,
		Endpoint:         rec.endpoint,
		Status:           status,
		HTTPStatus:       httpStatus,
		LatencyMs:        time.Since(rec.start).Milliseconds(),
		PromptTokens:     rec.prompt,
		CompletionTokens: rec.completion,
		Stream:           rec.stream,
		Proxy:            rec.proxy,
		Error:            errText,
	})
}

// sanitizeProxy keeps the proxy label useful without ever persisting
// credentials embedded in a proxy URL.
func sanitizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "default"
	}
	if strings.Contains(raw, "@") {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			u.User = nil
			return u.String()
		}
		return "redacted"
	}
	return raw
}

func numberOf(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	default:
		return 0
	}
}
