package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"opencode-router/internal/upstream"
)

// bridgeWriter keeps the existing fallback engine private while presenting only
// Anthropic Messages to callers. No HTTP loopback request or full-stream buffer.
type bridgeWriter struct {
	out    http.ResponseWriter
	header http.Header
	code   int
	body   bytes.Buffer
	stream *upstream.MessageStream
	err    error
}

func (w *bridgeWriter) Header() http.Header { return w.header }
func (w *bridgeWriter) WriteHeader(code int) {
	if w.code != 0 {
		return
	}
	w.code = code
	if w.stream != nil && code < 400 {
		for k, v := range w.header {
			w.out.Header()[k] = v
		}
		w.out.Header().Set("Content-Type", "text/event-stream")
		w.out.WriteHeader(code)
	}
}
func (w *bridgeWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.WriteHeader(200)
	}
	if w.stream != nil && w.code < 400 {
		n, err := w.stream.Write(p)
		w.err = err
		return n, err
	}
	return w.body.Write(p)
}
func (w *bridgeWriter) Flush() {
	if w.stream != nil && w.code > 0 && w.code < 400 {
		flusher(w.out)()
	}
}
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed", "invalid_request_error")
		return
	}
	body, err := decodeBody(r)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	chat, err := upstream.MessagesToChat(body)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	encoded, err := json.Marshal(chat)
	if err != nil {
		writeError(w, 400, err.Error(), "invalid_request_error")
		return
	}
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(encoded))
	inner.ContentLength = int64(len(encoded))
	adapter := &bridgeWriter{out: w, header: make(http.Header)}
	if body["stream"] == true {
		model, _ := body["model"].(string)
		adapter.stream = &upstream.MessageStream{W: w, Flush: flusher(w), Model: model}
	}
	s.handleChatCompletions(adapter, inner)
	if adapter.stream != nil && adapter.code < 400 {
		err := adapter.err
		if err == nil {
			err = adapter.stream.Finish()
		}
		if err != nil {
			b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}})
			_, _ = w.Write(append(append([]byte("event: error\ndata: "), b...), []byte("\n\n")...))
			flusher(w)()
		}
		return
	}
	for k, v := range adapter.header {
		w.Header()[k] = v
	}
	if adapter.code >= 400 {
		var payload map[string]any
		_ = json.Unmarshal(adapter.body.Bytes(), &payload)
		if payload == nil {
			payload = map[string]any{"error": map[string]any{"type": "api_error", "message": "invalid upstream error"}}
		}
		payload["type"] = "error"
		writeJSON(w, adapter.code, payload)
		return
	}
	var result map[string]any
	if err := json.Unmarshal(adapter.body.Bytes(), &result); err != nil {
		writeError(w, 502, "invalid upstream response", "api_error")
		return
	}
	result, err = upstream.ChatToMessage(result)
	if err != nil {
		writeError(w, 502, err.Error(), "api_error")
		return
	}
	writeJSON(w, 200, result)
}
