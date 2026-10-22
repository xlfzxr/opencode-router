package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"opencode-router/internal/upstream"
)

const maxBodyBytes = 16 << 20

type Server struct {
	Client *upstream.Client
}

func New(client *upstream.Client) *Server {
	return &Server{Client: client}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	return mux
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

	switch kind {
	case upstream.KindChat:
		s.proxyChat(w, r, body, model, stream)
	case upstream.KindResponses:
		s.proxyChatToResponses(w, r, body, model, stream)
	case upstream.KindMessages:
		s.proxyChatToMessages(w, r, body, model, stream)
	}
}

func (s *Server) proxyChat(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindChat, model, body, s.callOptions(r, stream))
	if err != nil {
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
		if err := upstream.StreamPassthrough(idle, w, flush, rename); err != nil {
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
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) proxyChatToResponses(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool) {
	body["model"] = model
	converted := upstream.ChatToResponses(body)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindResponses, model, converted, s.callOptions(r, stream))
	if err != nil {
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
		if err := upstream.StreamResponsesToChat(idle, w, flush, model, rename); err != nil && !idle.TimedOut() {
			log.Printf("[responses->chat] stream error: %v", err)
		}
		return
	}
	out, err := upstream.AggregateResponsesSSE(resp.Body, model)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, upstream.ResponsesToChat(out, model))
}

func (s *Server) proxyChatToMessages(w http.ResponseWriter, r *http.Request, body map[string]any, model string, stream bool) {
	body["model"] = model
	converted := upstream.ChatToAnthropic(body)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindMessages, model, converted, s.callOptions(r, stream))
	if err != nil {
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
		if err := upstream.StreamAnthropicToChat(idle, w, flush, model, rename); err != nil && !idle.TimedOut() {
			log.Printf("[messages->chat] stream error: %v", err)
		}
		return
	}
	out, err := upstream.AggregateAnthropicSSE(resp.Body, model)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, upstream.AnthropicToChat(out, model))
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindResponses, model, body, s.callOptions(r, stream))
	if err != nil {
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
		if err := upstream.StreamPassthrough(idle, w, flush, rename); err != nil && !idle.TimedOut() {
			log.Printf("[responses] stream error: %v", err)
		}
		return
	}
	out, err := upstream.AggregateResponsesSSE(resp.Body, model)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	resp, rename, err := s.Client.Do(ctx, upstream.KindMessages, model, body, s.callOptions(r, stream))
	if err != nil {
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
		if err := upstream.StreamPassthrough(idle, w, flush, rename); err != nil && !idle.TimedOut() {
			log.Printf("[messages] stream error: %v", err)
		}
		return
	}
	out, err := upstream.AggregateAnthropicSSE(resp.Body, model)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
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
