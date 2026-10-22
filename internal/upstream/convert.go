package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ChatToResponses converts an OpenAI Chat Completions request body to the
// Responses API shape used by the muse-spark family.
func ChatToResponses(body map[string]any) map[string]any {
	out := map[string]any{
		"model":  body["model"],
		"stream": true,
		"store":  false,
	}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	if v, ok := body["max_completion_tokens"]; ok {
		out["max_output_tokens"] = v
	} else if v, ok := body["max_tokens"]; ok {
		out["max_output_tokens"] = v
	}
	if v, ok := body["reasoning_effort"]; ok {
		out["reasoning_effort"] = v
	}

	var instructions []string
	input := []any{}
	messages, _ := body["messages"].([]any)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			instructions = append(instructions, textOf(msg["content"]))
		case "user", "assistant":
			input = append(input, map[string]any{
				"type": "message",
				"role": role,
				"content": []any{map[string]any{
					"type": inputTextType(role),
					"text": textOf(msg["content"]),
				}},
			})
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := call["function"].(map[string]any)
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   fmt.Sprint(call["id"]),
						"name":      fmt.Sprint(fn["name"]),
						"arguments": coerceJSONString(fn["arguments"], "{}"),
					})
				}
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": fmt.Sprint(msg["tool_call_id"]),
				"output":  textOf(msg["content"]),
			})
		}
	}
	if len(instructions) > 0 {
		out["instructions"] = strings.Join(instructions, "\n\n")
	}
	if len(input) == 0 {
		input = []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "..."}},
		}}
	}
	out["input"] = input

	if tools, ok := body["tools"].([]any); ok {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			name := fmt.Sprint(fn["name"])
			if strings.TrimSpace(name) == "" {
				continue
			}
			converted = append(converted, map[string]any{
				"type":        "function",
				"name":        name,
				"description": fn["description"],
				"parameters":  fn["parameters"],
				"strict":      false,
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			out["tool_choice"] = map[string]any{"type": "function", "name": fn["name"]}
		}
	} else if v, ok := body["tool_choice"]; ok {
		out["tool_choice"] = v
	}
	return out
}

func inputTextType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

// ResponsesToChat converts a completed Responses API object to a Chat
// Completions object for OpenAI-compatible downstream clients.
func ResponsesToChat(resp map[string]any, model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": ""}
	var content strings.Builder
	calls := []any{}
	reasoning := strings.Builder{}

	output, _ := resp["output"].([]any)
	for _, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["type"] {
		case "message":
			content.WriteString(textOf(item["content"]))
		case "reasoning":
			reasoning.WriteString(textOf(item["summary"]))
		case "function_call":
			name := fmt.Sprint(item["name"])
			if original, ok := mapAlias(item, "original_name"); ok {
				name = original
			}
			calls = append(calls, map[string]any{
				"index": len(calls),
				"id":    fmt.Sprint(item["call_id"]),
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": coerceJSONString(item["arguments"], "{}"),
				},
			})
		}
	}
	message["content"] = content.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	} else if status, _ := resp["status"].(string); status == "incomplete" {
		finish = "length"
	}
	out := map[string]any{
		"id":      stringOr(resp["id"], "chatcmpl-"+randomBase62(24)),
		"object":  "chat.completion",
		"created": numberOf(resp["created_at"]),
		"model":   stringOr(resp["model"], model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage, ok := resp["usage"].(map[string]any); ok {
		in := int(numberOf(usage["input_tokens"]))
		outTokens := int(numberOf(usage["output_tokens"]))
		out["usage"] = map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTokens,
			"total_tokens":      in + outTokens,
		}
	}
	return out
}

func mapAlias(item map[string]any, key string) (string, bool) {
	v, ok := item[key].(string)
	return v, ok
}

// ChatToAnthropic converts an OpenAI Chat Completions request to the Messages
// API shape used by union-alpha.
func ChatToAnthropic(body map[string]any) map[string]any {
	out := map[string]any{
		"model":  body["model"],
		"stream": true,
	}
	maxTokens := 4096
	if v, ok := body["max_completion_tokens"]; ok {
		maxTokens = int(numberOf(v))
	} else if v, ok := body["max_tokens"]; ok {
		maxTokens = int(numberOf(v))
	}
	out["max_tokens"] = maxTokens
	for _, key := range []string{"temperature", "top_p"} {
		if v, ok := body[key]; ok {
			out[key] = v
		}
	}
	if v, ok := body["stop"]; ok {
		switch stop := v.(type) {
		case string:
			out["stop_sequences"] = []any{stop}
		case []any:
			out["stop_sequences"] = stop
		}
	}

	var system []string
	messages := []any{}
	rawMessages, _ := body["messages"].([]any)
	for _, raw := range rawMessages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "system", "developer":
			system = append(system, textOf(msg["content"]))
		case "tool":
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": fmt.Sprint(msg["tool_call_id"]),
					"content":     textOf(msg["content"]),
				}},
			})
		case "user":
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text",
					"text": textOf(msg["content"]),
				}},
			})
		case "assistant":
			blocks := []any{}
			if text := textOf(msg["content"]); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					fn, _ := call["function"].(map[string]any)
					var input any = map[string]any{}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						_ = json.Unmarshal([]byte(args), &input)
					}
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    fmt.Sprint(call["id"]),
						"name":  fmt.Sprint(fn["name"]),
						"input": input,
					})
				}
			}
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "assistant", "content": blocks})
			}
		}
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	if len(messages) == 0 {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "..."}},
		})
	}
	out["messages"] = messages

	if tools, ok := body["tools"].([]any); ok {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := tool["function"].(map[string]any)
			name := fmt.Sprint(fn["name"])
			if strings.TrimSpace(name) == "" {
				continue
			}
			converted = append(converted, map[string]any{
				"name":         name,
				"description":  fn["description"],
				"input_schema": fn["parameters"],
			})
		}
		if len(converted) > 0 {
			out["tools"] = converted
		}
	}
	switch choice := body["tool_choice"].(type) {
	case string:
		switch choice {
		case "auto":
			out["tool_choice"] = map[string]any{"type": "auto"}
		case "required":
			out["tool_choice"] = map[string]any{"type": "any"}
		}
	case map[string]any:
		if fn, ok := choice["function"].(map[string]any); ok {
			out["tool_choice"] = map[string]any{"type": "tool", "name": fn["name"]}
		}
	}
	return out
}

// AnthropicToChat converts a Messages API object to Chat Completions.
func AnthropicToChat(resp map[string]any, model string) map[string]any {
	message := map[string]any{"role": "assistant", "content": ""}
	var text strings.Builder
	var reasoning strings.Builder
	calls := []any{}

	content, _ := resp["content"].([]any)
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch block["type"] {
		case "text":
			text.WriteString(fmt.Sprint(block["text"]))
		case "thinking":
			reasoning.WriteString(fmt.Sprint(block["thinking"]))
		case "tool_use":
			args, _ := json.Marshal(block["input"])
			calls = append(calls, map[string]any{
				"index": len(calls),
				"id":    fmt.Sprint(block["id"]),
				"type":  "function",
				"function": map[string]any{
					"name":      fmt.Sprint(block["name"]),
					"arguments": string(args),
				},
			})
		}
	}
	message["content"] = text.String()
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	finish := mapAnthropicStop(fmt.Sprint(resp["stop_reason"]))
	out := map[string]any{
		"id":      stringOr(resp["id"], "chatcmpl-"+randomBase62(24)),
		"object":  "chat.completion",
		"created": numberOf(resp["created_at"]),
		"model":   stringOr(resp["model"], model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	}
	if usage, ok := resp["usage"].(map[string]any); ok {
		in := int(numberOf(usage["input_tokens"]))
		outTokens := int(numberOf(usage["output_tokens"]))
		out["usage"] = map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTokens,
			"total_tokens":      in + outTokens,
		}
	}
	return out
}

func mapAnthropicStop(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "":
		return "stop"
	default:
		return "stop"
	}
}

func textOf(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case []any:
		var b strings.Builder
		for _, part := range value {
			if m, ok := part.(map[string]any); ok {
				if text, ok := m["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func stringOr(v any, fallback string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return fallback
}

func writeSSE(w io.Writer, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", data)
	return err
}

func writeDone(w io.Writer) {
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// reportUsage forwards token counts from a passthrough chunk that carries a
// usage object. Chat Completions normally sends usage in a final chunk.
func reportUsage(payload map[string]any, fn func(int, int)) {
	if fn == nil {
		return
	}
	u, ok := payload["usage"].(map[string]any)
	if !ok {
		return
	}
	p, c := usageNumbers(u)
	if p > 0 || c > 0 {
		fn(p, c)
	}
}

func usageNumbers(u map[string]any) (int, int) {
	prompt := int(numberOf(u["prompt_tokens"]))
	completion := int(numberOf(u["completion_tokens"]))
	if prompt == 0 {
		prompt = int(numberOf(u["input_tokens"]))
	}
	if completion == 0 {
		completion = int(numberOf(u["output_tokens"]))
	}
	return prompt, completion
}

// StreamPassthrough relays upstream SSE, restoring concealed tool names and
// flushing after every event so tokens reach the client immediately.
func StreamPassthrough(rd io.Reader, w io.Writer, flush func(), rename map[string]string, usageFn func(int, int)) error {
	return ScanSSE(rd, func(ev sseEvent) error {
		data := strings.TrimSpace(string(ev.Data))
		if data == "" {
			return nil
		}
		if data == "[DONE]" {
			writeDone(w)
			flush()
			return nil
		}
		var payload any
		if json.Unmarshal([]byte(data), &payload) == nil {
			if obj, ok := payload.(map[string]any); ok {
				reportUsage(obj, usageFn)
			}
			payload = RestoreToolNames(payload, rename)
			encoded, err := json.Marshal(payload)
			if err == nil {
				data = string(encoded)
			}
		}
		if ev.Event != "" {
			if _, err := fmt.Fprintf(w, "event: %s\n", ev.Event); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return err
		}
		flush()
		return nil
	})
}

// StreamResponsesToChat translates Responses SSE events into Chat Completions
// chunks so an OpenAI client can consume a muse-spark stream.
func StreamResponsesToChat(rd io.Reader, w io.Writer, flush func(), model string, rename map[string]string, usageFn func(int, int)) error {
	id := "chatcmpl-" + randomBase62(24)
	toolIndex := -1
	finish := "stop"
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
			text, _ := payload["delta"].(string)
			if text == "" {
				return nil
			}
			chunk := map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": text},
				}},
			}
			if err := writeSSE(w, chunk); err != nil {
				return err
			}
			flush()
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			if item == nil || item["type"] != "function_call" {
				return nil
			}
			toolIndex++
			name := fmt.Sprint(item["name"])
			if original, ok := rename[name]; ok {
				name = original
			}
			finish = "tool_calls"
			chunk := map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIndex,
						"id":    fmt.Sprint(item["call_id"]),
						"type":  "function",
						"function": map[string]any{
							"name":      name,
							"arguments": coerceJSONString(item["arguments"], "{}"),
						},
					}}},
				}},
			}
			if err := writeSSE(w, chunk); err != nil {
				return err
			}
			flush()
		case "response.completed":
			if response, ok := payload["response"].(map[string]any); ok {
				if u, ok := response["usage"].(map[string]any); ok {
					usage = u
				}
			}
		case "response.failed", "error":
			return fmt.Errorf("upstream stream error: %s", canonicalJSON(payload))
		}
		return nil
	})
	if err != nil {
		return err
	}
	final := map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	if len(usage) > 0 {
		in := int(numberOf(usage["input_tokens"]))
		outTokens := int(numberOf(usage["output_tokens"]))
		if usageFn != nil && (in > 0 || outTokens > 0) {
			usageFn(in, outTokens)
		}
		final["usage"] = map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTokens,
			"total_tokens":      in + outTokens,
		}
	}
	if err := writeSSE(w, final); err != nil {
		return err
	}
	writeDone(w)
	flush()
	return nil
}

// StreamAnthropicToChat translates Messages SSE events into Chat Completions
// chunks for union-alpha.
func StreamAnthropicToChat(rd io.Reader, w io.Writer, flush func(), model string, rename map[string]string, usageFn func(int, int)) error {
	id := "chatcmpl-" + randomBase62(24)
	toolIndex := -1
	finish := "stop"
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
		case "content_block_start":
			block, _ := payload["content_block"].(map[string]any)
			if block == nil || block["type"] != "tool_use" {
				return nil
			}
			toolIndex++
			name := fmt.Sprint(block["name"])
			if original, ok := rename[name]; ok {
				name = original
			}
			finish = "tool_calls"
			chunk := map[string]any{
				"id": id, "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIndex,
						"id":    fmt.Sprint(block["id"]),
						"type":  "function",
						"function": map[string]any{
							"name":      name,
							"arguments": "",
						},
					}}},
				}},
			}
			if err := writeSSE(w, chunk); err != nil {
				return err
			}
			flush()
		case "content_block_delta":
			delta, _ := payload["delta"].(map[string]any)
			if delta == nil {
				return nil
			}
			switch delta["type"] {
			case "text_delta":
				chunk := map[string]any{
					"id": id, "object": "chat.completion.chunk", "model": model,
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"content": fmt.Sprint(delta["text"])},
					}},
				}
				if err := writeSSE(w, chunk); err != nil {
					return err
				}
				flush()
			case "input_json_delta":
				partial := fmt.Sprint(delta["partial_json"])
				if partial == "" || toolIndex < 0 {
					return nil
				}
				chunk := map[string]any{
					"id": id, "object": "chat.completion.chunk", "model": model,
					"choices": []any{map[string]any{
						"index": 0,
						"delta": map[string]any{"tool_calls": []any{map[string]any{
							"index":    toolIndex,
							"function": map[string]any{"arguments": partial},
						}}},
					}},
				}
				if err := writeSSE(w, chunk); err != nil {
					return err
				}
				flush()
			}
		case "message_delta":
			if delta, ok := payload["delta"].(map[string]any); ok {
				if reason, ok := delta["stop_reason"].(string); ok && reason != "" {
					finish = mapAnthropicStop(reason)
				}
			}
			if u, ok := payload["usage"].(map[string]any); ok {
				usage = u
			}
		case "error":
			return fmt.Errorf("upstream stream error: %s", canonicalJSON(payload))
		}
		return nil
	})
	if err != nil {
		return err
	}
	final := map[string]any{
		"id": id, "object": "chat.completion.chunk", "model": model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	if len(usage) > 0 {
		outTokens := int(numberOf(usage["output_tokens"]))
		in := int(numberOf(usage["input_tokens"]))
		if usageFn != nil && (in > 0 || outTokens > 0) {
			usageFn(in, outTokens)
		}
		final["usage"] = map[string]any{
			"prompt_tokens":     in,
			"completion_tokens": outTokens,
			"total_tokens":      in + outTokens,
		}
	}
	if err := writeSSE(w, final); err != nil {
		return err
	}
	writeDone(w)
	flush()
	return nil
}
