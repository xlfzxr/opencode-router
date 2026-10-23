package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// MessagesToChat is the internal wire adapter, not a public OpenAI endpoint.
// Unknown content is rejected rather than silently losing user input.
func MessagesToChat(body map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for _, k := range []string{"model", "stream", "max_tokens", "temperature", "top_p"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if n, ok := body["max_tokens"].(float64); !ok || n <= 0 || n != float64(int64(n)) {
		return nil, fmt.Errorf("max_tokens is required")
	}
	if v, ok := body["stop_sequences"]; ok {
		out["stop"] = v
	}
	messages := []any{}
	text := func(v any) (string, error) {
		if v == nil {
			return "", nil
		}
		if s, ok := v.(string); ok {
			return s, nil
		}
		blocks, ok := v.([]any)
		if !ok {
			return "", fmt.Errorf("expected text blocks")
		}
		parts := []string{}
		for _, b := range blocks {
			m, ok := b.(map[string]any)
			if !ok || m["type"] != "text" {
				return "", fmt.Errorf("expected text block")
			}
			s, _ := m["text"].(string)
			parts = append(parts, s)
		}
		return strings.Join(parts, "\n"), nil
	}
	if v, ok := body["system"]; ok {
		s, err := text(v)
		if err != nil {
			return nil, err
		}
		messages = append(messages, map[string]any{"role": "system", "content": s})
	}
	raw, ok := body["messages"].([]any)
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("messages must be a nonempty array")
	}
	for _, v := range raw {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid message")
		}
		role, _ := m["role"].(string)
		if role != "user" && role != "assistant" {
			return nil, fmt.Errorf("invalid message role")
		}
		if s, ok := m["content"].(string); ok {
			messages = append(messages, map[string]any{"role": role, "content": s})
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			return nil, fmt.Errorf("invalid message content")
		}
		content := []any{}
		calls := []any{}
		results := []any{}
		for _, v := range blocks {
			b, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid content block")
			}
			switch b["type"] {
			case "text":
				content = append(content, map[string]any{"type": "text", "text": b["text"]})
			case "image":
				src, _ := b["source"].(map[string]any)
				var url string
				switch src["type"] {
				case "base64":
					url = fmt.Sprintf("data:%s;base64,%s", src["media_type"], src["data"])
				case "url":
					url, _ = src["url"].(string)
				default:
					return nil, fmt.Errorf("unsupported image source")
				}
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			case "tool_use":
				if role != "assistant" {
					return nil, fmt.Errorf("tool_use requires assistant")
				}
				args, err := json.Marshal(b["input"])
				if err != nil {
					return nil, err
				}
				calls = append(calls, map[string]any{"id": b["id"], "type": "function", "function": map[string]any{"name": b["name"], "arguments": string(args)}})
			case "tool_result":
				if role != "user" {
					return nil, fmt.Errorf("tool_result requires user")
				}
				s, err := text(b["content"])
				if err != nil {
					return nil, err
				}
				results = append(results, map[string]any{"role": "tool", "tool_call_id": b["tool_use_id"], "content": s})
			case "thinking", "redacted_thinking": // Native signatures cannot be replayed across providers.
			default:
				return nil, fmt.Errorf("unsupported content block %v", b["type"])
			}
		}
		messages = append(messages, results...)
		if len(content) > 0 || len(calls) > 0 {
			msg := map[string]any{"role": role, "content": content}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		}
	}
	out["messages"] = messages
	if tools, ok := body["tools"].([]any); ok {
		converted := []any{}
		for _, v := range tools {
			t, ok := v.(map[string]any)
			if !ok || t["name"] == nil || t["input_schema"] == nil || (t["type"] != nil && t["type"] != "custom") {
				return nil, fmt.Errorf("unsupported tool")
			}
			converted = append(converted, map[string]any{"type": "function", "function": map[string]any{"name": t["name"], "description": t["description"], "parameters": t["input_schema"]}})
		}
		out["tools"] = converted
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		switch choice["type"] {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": choice["name"]}}
		default:
			return nil, fmt.Errorf("unsupported tool_choice")
		}
		if choice["disable_parallel_tool_use"] == true {
			out["parallel_tool_calls"] = false
		}
	}
	return out, nil
}
func stopReason(v any) string {
	switch v {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}
func ChatToMessage(body map[string]any) (map[string]any, error) {
	choices, _ := body["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("missing upstream choices")
	}
	c, _ := choices[0].(map[string]any)
	m, _ := c["message"].(map[string]any)
	content := []any{}
	if s, ok := m["content"].(string); ok && s != "" {
		content = append(content, map[string]any{"type": "text", "text": s})
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, v := range calls {
			call, _ := v.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			args, _ := fn["arguments"].(string)
			var input map[string]any
			if err := json.Unmarshal([]byte(args), &input); err != nil || input == nil {
				return nil, fmt.Errorf("invalid tool input from upstream")
			}
			content = append(content, map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": input})
		}
	}
	usage, _ := body["usage"].(map[string]any)
	return map[string]any{"id": body["id"], "type": "message", "role": "assistant", "model": body["model"], "content": content, "stop_reason": stopReason(c["finish_reason"]), "stop_sequence": nil, "usage": map[string]any{"input_tokens": numberOf(usage["prompt_tokens"]), "output_tokens": numberOf(usage["completion_tokens"])}}, nil
}

// MessageStream incrementally maps internal Chat SSE to Anthropic events.
type MessageStream struct {
	W             io.Writer
	Flush         func()
	Model         string
	pending       string
	started, done bool
	textIndex     int
	blocks        int
	tools         map[int]int
	input, output float64
	reason        string
}

func (s *MessageStream) emit(kind string, p map[string]any) error {
	p["type"] = kind
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.W, "event: %s\ndata: %s\n\n", kind, b)
	if s.Flush != nil {
		s.Flush()
	}
	return err
}
func (s *MessageStream) Write(p []byte) (int, error) {
	s.pending += string(p)
	for {
		end := strings.Index(s.pending, "\n\n")
		if end < 0 {
			break
		}
		frame := s.pending[:end]
		s.pending = s.pending[end+2:]
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				if err := s.chunk(strings.TrimSpace(strings.TrimPrefix(line, "data:"))); err != nil {
					return 0, err
				}
			}
		}
	}
	return len(p), nil
}
func (s *MessageStream) chunk(data string) error {
	if s.done {
		return nil
	}
	if data == "[DONE]" {
		s.done = true
		if !s.started {
			return fmt.Errorf("empty stream")
		}
		for i := 0; i < s.blocks; i++ {
			if err := s.emit("content_block_stop", map[string]any{"index": i}); err != nil {
				return err
			}
		}
		if err := s.emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": s.reason, "stop_sequence": nil}, "usage": map[string]any{"input_tokens": s.input, "output_tokens": s.output}}); err != nil {
			return err
		}
		return s.emit("message_stop", map[string]any{})
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		return err
	}
	if p["error"] != nil {
		return s.emit("error", map[string]any{"error": p["error"]})
	}
	if !s.started {
		s.started = true
		s.textIndex = -1
		s.tools = map[int]int{}
		s.reason = "end_turn"
		if model, ok := p["model"].(string); ok {
			s.Model = model
		}
		if err := s.emit("message_start", map[string]any{"message": map[string]any{"id": p["id"], "type": "message", "role": "assistant", "model": s.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 0, "output_tokens": 0}}}); err != nil {
			return err
		}
	}
	if u, ok := p["usage"].(map[string]any); ok {
		s.input = numberOf(u["prompt_tokens"])
		s.output = numberOf(u["completion_tokens"])
	}
	choices, _ := p["choices"].([]any)
	for _, v := range choices {
		c, _ := v.(map[string]any)
		if c["finish_reason"] != nil {
			s.reason = stopReason(c["finish_reason"])
		}
		d, _ := c["delta"].(map[string]any)
		if text, ok := d["content"].(string); ok && text != "" {
			if s.textIndex < 0 {
				s.textIndex = s.blocks
				s.blocks++
				if err := s.emit("content_block_start", map[string]any{"index": s.textIndex, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
					return err
				}
			}
			if err := s.emit("content_block_delta", map[string]any{"index": s.textIndex, "delta": map[string]any{"type": "text_delta", "text": text}}); err != nil {
				return err
			}
		}
		calls, _ := d["tool_calls"].([]any)
		for _, v := range calls {
			call, _ := v.(map[string]any)
			index := int(numberOf(call["index"]))
			fn, _ := call["function"].(map[string]any)
			block, exists := s.tools[index]
			if !exists {
				if call["id"] == nil || fn["name"] == nil {
					return fmt.Errorf("tool stream missing id/name")
				}
				block = s.blocks
				s.blocks++
				s.tools[index] = block
				if err := s.emit("content_block_start", map[string]any{"index": block, "content_block": map[string]any{"type": "tool_use", "id": call["id"], "name": fn["name"], "input": map[string]any{}}}); err != nil {
					return err
				}
			}
			if args, ok := fn["arguments"].(string); ok && args != "" {
				if err := s.emit("content_block_delta", map[string]any{"index": block, "delta": map[string]any{"type": "input_json_delta", "partial_json": args}}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func (s *MessageStream) Finish() error {
	if !s.done {
		return fmt.Errorf("upstream stream ended without completion")
	}
	return nil
}
