package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"
)

// IdleReader cancels the request context when the upstream stops producing
// bytes for longer than d. A stalled stream is the main failure mode at high
// token rates, so it must fail fast instead of hanging forever.
type IdleReader struct {
	rd       io.Reader
	d        time.Duration
	cancel   context.CancelFunc
	timer    *time.Timer
	timedOut atomic.Bool
	stopped  atomic.Bool
}

func NewIdleReader(rd io.Reader, d time.Duration, cancel context.CancelFunc) *IdleReader {
	r := &IdleReader{rd: rd, d: d, cancel: cancel}
	if d > 0 {
		r.timer = time.AfterFunc(d, func() {
			if r.stopped.Load() {
				return
			}
			r.timedOut.Store(true)
			if r.cancel != nil {
				r.cancel()
			}
		})
	}
	return r
}

func (r *IdleReader) Read(p []byte) (int, error) {
	n, err := r.rd.Read(p)
	if n > 0 && r.timer != nil {
		r.timer.Reset(r.d)
	}
	if err != nil {
		r.Stop()
	}
	return n, err
}

func (r *IdleReader) Stop() {
	r.stopped.Store(true)
	if r.timer != nil {
		r.timer.Stop()
	}
}

func (r *IdleReader) TimedOut() bool { return r.timedOut.Load() }

type sseEvent struct {
	Event string
	Data  []byte
}

func ScanSSE(rd io.Reader, fn func(sseEvent) error) error {
	scanner := bufio.NewScanner(rd)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var event string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				if err := fn(sseEvent{Event: event, Data: []byte(strings.Join(data, "\n"))}); err != nil {
					return err
				}
			}
			event = ""
			data = data[:0]
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line, "data:"))
		}
	}
	if len(data) > 0 {
		return fn(sseEvent{Event: event, Data: []byte(strings.Join(data, "\n"))})
	}
	return scanner.Err()
}

type toolCallAgg struct {
	index     int
	id        string
	name      string
	arguments strings.Builder
}

type chatAgg struct {
	id        string
	model     string
	created   int64
	content   strings.Builder
	reasoning strings.Builder
	tools     map[int]*toolCallAgg
	order     []int
	finish    string
	usage     map[string]any
}

func AggregateChatSSE(rd io.Reader, model string) (map[string]any, error) {
	agg := &chatAgg{id: "chatcmpl-" + randomBase62(24), model: model, created: time.Now().Unix(), tools: map[int]*toolCallAgg{}}
	err := ScanSSE(rd, func(ev sseEvent) error {
		data := strings.TrimSpace(string(ev.Data))
		if data == "" || data == "[DONE]" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		if errObj, ok := payload["error"].(map[string]any); ok {
			return fmt.Errorf("upstream stream error: %s", canonicalJSON(errObj))
		}
		if id, ok := payload["id"].(string); ok && id != "" {
			agg.id = id
		}
		if m, ok := payload["model"].(string); ok && m != "" {
			agg.model = m
		}
		if u, ok := payload["usage"].(map[string]any); ok {
			agg.usage = u
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			return nil
		}
		choice, _ := choices[0].(map[string]any)
		if finish, ok := choice["finish_reason"].(string); ok && finish != "" {
			agg.finish = finish
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta, _ = choice["message"].(map[string]any)
		}
		if delta == nil {
			return nil
		}
		appendText(&agg.content, delta["content"])
		appendText(&agg.reasoning, delta["reasoning_content"])
		if calls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				index := int(numberOf(call["index"]))
				entry, ok := agg.tools[index]
				if !ok {
					entry = &toolCallAgg{index: index}
					agg.tools[index] = entry
					agg.order = append(agg.order, index)
				}
				if id, ok := call["id"].(string); ok && id != "" {
					entry.id = id
				}
				if fn, ok := call["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok && name != "" {
						entry.name = name
					}
					appendText(&entry.arguments, fn["arguments"])
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	message := map[string]any{"role": "assistant", "content": agg.content.String()}
	if agg.reasoning.Len() > 0 {
		message["reasoning_content"] = agg.reasoning.String()
	}
	if len(agg.order) > 0 {
		calls := make([]any, 0, len(agg.order))
		for _, index := range agg.order {
			entry := agg.tools[index]
			calls = append(calls, map[string]any{
				"index": entry.index,
				"id":    entry.id,
				"type":  "function",
				"function": map[string]any{
					"name":      entry.name,
					"arguments": entry.arguments.String(),
				},
			})
		}
		message["tool_calls"] = calls
	}
	finish := agg.finish
	if finish == "" {
		if len(agg.order) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	out := map[string]any{
		"id":      agg.id,
		"object":  "chat.completion",
		"created": agg.created,
		"model":   agg.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if agg.usage != nil {
		out["usage"] = agg.usage
	}
	return out, nil
}

func AggregateAnthropicSSE(rd io.Reader, model string) (map[string]any, error) {
	var text strings.Builder
	var id string
	inputTokens, outputTokens := 0, 0
	stopReason := "end_turn"
	tools := map[int]*toolCallAgg{}
	order := []int{}

	err := ScanSSE(rd, func(ev sseEvent) error {
		data := strings.TrimSpace(string(ev.Data))
		if data == "" || data == "[DONE]" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		switch payload["type"] {
		case "message_start":
			if msg, ok := payload["message"].(map[string]any); ok {
				if v, ok := msg["id"].(string); ok {
					id = v
				}
				if usage, ok := msg["usage"].(map[string]any); ok {
					inputTokens = int(numberOf(usage["input_tokens"]))
					outputTokens = int(numberOf(usage["output_tokens"]))
				}
			}
		case "content_block_start":
			index := int(numberOf(payload["index"]))
			block, _ := payload["content_block"].(map[string]any)
			if block != nil && block["type"] == "tool_use" {
				name, _ := block["name"].(string)
				tools[index] = &toolCallAgg{index: index, id: fmt.Sprint(block["id"]), name: name}
				order = append(order, index)
			}
		case "content_block_delta":
			index := int(numberOf(payload["index"]))
			delta, _ := payload["delta"].(map[string]any)
			if delta == nil {
				return nil
			}
			switch delta["type"] {
			case "text_delta":
				appendText(&text, delta["text"])
			case "input_json_delta":
				if entry, ok := tools[index]; ok {
					appendText(&entry.arguments, delta["partial_json"])
				}
			}
		case "message_delta":
			if delta, ok := payload["delta"].(map[string]any); ok {
				if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
					stopReason = reason
				}
			}
			if usage, ok := payload["usage"].(map[string]any); ok {
				outputTokens = int(numberOf(usage["output_tokens"]))
			}
		case "error":
			return fmt.Errorf("upstream stream error: %s", canonicalJSON(payload))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	content := []any{}
	if text.Len() > 0 {
		content = append(content, map[string]any{"type": "text", "text": text.String()})
	}
	for _, index := range order {
		entry := tools[index]
		var input any = map[string]any{}
		if entry.arguments.Len() > 0 {
			_ = json.Unmarshal([]byte(entry.arguments.String()), &input)
		}
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    entry.id,
			"name":  entry.name,
			"input": input,
		})
	}
	if id == "" {
		id = "msg_" + randomBase62(24)
	}
	return map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     content,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}, nil
}

func AggregateResponsesSSE(rd io.Reader, model string) (map[string]any, error) {
	var final map[string]any
	var text strings.Builder
	output := []any{}
	usage := map[string]any{}

	err := ScanSSE(rd, func(ev sseEvent) error {
		data := strings.TrimSpace(string(ev.Data))
		if data == "" || data == "[DONE]" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		switch payload["type"] {
		case "response.output_text.delta":
			appendText(&text, payload["delta"])
		case "response.output_item.done":
			if item, ok := payload["item"].(map[string]any); ok {
				output = append(output, item)
			}
		case "response.completed":
			if response, ok := payload["response"].(map[string]any); ok {
				final = response
			}
		case "response.failed", "error":
			return fmt.Errorf("upstream stream error: %s", canonicalJSON(payload))
		}
		if u, ok := payload["response"].(map[string]any); ok {
			if v, ok := u["usage"].(map[string]any); ok {
				usage = v
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if final != nil {
		return final, nil
	}
	if len(output) == 0 && text.Len() > 0 {
		output = append(output, map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text",
				"text": text.String(),
			}},
		})
	}
	return map[string]any{
		"id":     "resp_" + randomBase62(24),
		"object": "response",
		"model":  model,
		"output": output,
		"status": "completed",
		"usage":  usage,
	}, nil
}

func appendText(builder *strings.Builder, v any) {
	switch value := v.(type) {
	case string:
		builder.WriteString(value)
	case []any:
		for _, part := range value {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					builder.WriteString(text)
				}
			}
		}
	}
}

func numberOf(v any) float64 {
	switch value := v.(type) {
	case float64:
		return value
	case int:
		return float64(value)
	case int64:
		return float64(value)
	case json.Number:
		f, _ := value.Float64()
		return f
	default:
		return 0
	}
}
