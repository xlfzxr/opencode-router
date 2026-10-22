package upstream

import (
	"encoding/json"
	"strings"
)

// Free-tier fingerprint tools. Upstream compares description and parameter
// schema verbatim; do not reword or reformat these.
var fingerprintOrder = []string{"read", "shell"}

var fingerprintDefs = map[string]toolDef{
	"read": {
		Description: "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File or directory to read"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	},
	"shell": {
		Description: "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	},
}

type toolDef struct {
	Description string
	Parameters  map[string]any
}

func fingerprintKey(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, key := range fingerprintOrder {
		if trimmed == key {
			return key
		}
	}
	return ""
}

func toolNameOf(tool any) string {
	m, ok := tool.(map[string]any)
	if !ok {
		return ""
	}
	if name, ok := m["name"].(string); ok && strings.TrimSpace(name) != "" {
		return strings.TrimSpace(name)
	}
	if fn, ok := m["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			return strings.TrimSpace(name)
		}
	}
	return ""
}

func chatFunctionOf(tool map[string]any) map[string]any {
	fn, _ := tool["function"].(map[string]any)
	return fn
}

func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func isFingerprintDefinition(tool map[string]any, key string) bool {
	def := chatFunctionOf(tool)
	if def == nil {
		def = tool
	}
	want := fingerprintDefs[key]
	desc, _ := def["description"].(string)
	return desc == want.Description && canonicalJSON(def["parameters"]) == canonicalJSON(want.Parameters)
}

func withToolName(tool map[string]any, name string) map[string]any {
	out := make(map[string]any, len(tool))
	for k, v := range tool {
		out[k] = v
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		copied := make(map[string]any, len(fn))
		for k, v := range fn {
			copied[k] = v
		}
		copied["name"] = name
		out["function"] = copied
		return out
	}
	out["name"] = name
	return out
}

// ApplyFingerprintTools aliases conflicting caller tools, appends the exact
// OpenCode definitions, retargets tool_choice and prior tool calls, and adds the
// placeholder note. flat=true for the Responses API tool shape.
func ApplyFingerprintTools(body map[string]any, flat bool) map[string]string {
	rename := make(map[string]string)
	if body == nil {
		return rename
	}
	tools, _ := body["tools"].([]any)

	taken := make(map[string]bool, len(tools)+2)
	seen := make(map[string]bool, len(tools))
	kept := make([]any, 0, len(tools)+2)

	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		name := toolNameOf(tool)
		if name != "" {
			taken[name] = true
		}
		key := fingerprintKey(name)
		if key == "" {
			kept = append(kept, tool)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if isFingerprintDefinition(tool, key) {
			kept = append(kept, tool)
			continue
		}
		alias := key + "_client"
		for i := 2; taken[alias]; i++ {
			alias = key + "_client" + itoa(i)
		}
		taken[alias] = true
		rename[alias] = key
		kept = append(kept, withToolName(tool, alias))
	}

	appended := make([]string, 0, 2)
	for _, name := range fingerprintOrder {
		if seen[name] {
			continue
		}
		def := fingerprintDefs[name]
		if flat {
			kept = append(kept, map[string]any{
				"type":        "function",
				"name":        name,
				"description": def.Description,
				"parameters":  def.Parameters,
				"strict":      false,
			})
		} else {
			kept = append(kept, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": def.Description,
					"parameters":  def.Parameters,
				},
			})
		}
		appended = append(appended, name)
	}
	body["tools"] = kept

	retargetToolChoice(body, rename)
	aliasHistoryToolCalls(body, rename)
	if len(appended) > 0 {
		noteUnavailableTools(body, flat, appended)
	}
	if flat {
		if _, ok := body["tool_choice"]; !ok {
			body["tool_choice"] = "auto"
		}
	}
	return rename
}

func retargetToolChoice(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	aliases := aliasesByOriginal(rename)
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if name, ok := choice["name"].(string); ok {
		if alias, ok := aliases[strings.TrimSpace(name)]; ok {
			copied := cloneMap(choice)
			copied["name"] = alias
			body["tool_choice"] = copied
		}
		return
	}
	if fn, ok := choice["function"].(map[string]any); ok {
		if name, ok := fn["name"].(string); ok {
			if alias, ok := aliases[strings.TrimSpace(name)]; ok {
				copiedChoice := cloneMap(choice)
				copiedFn := cloneMap(fn)
				copiedFn["name"] = alias
				copiedChoice["function"] = copiedFn
				body["tool_choice"] = copiedChoice
			}
		}
	}
}

func aliasHistoryToolCalls(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	aliases := aliasesByOriginal(rename)
	if input, ok := body["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if item["type"] == "function_call" {
				if name, ok := item["name"].(string); ok {
					if alias, ok := aliases[name]; ok {
						item["name"] = alias
					}
				}
			}
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			calls, ok := msg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := fn["name"].(string)
				if alias, ok := aliases[name]; ok {
					fn["name"] = alias
				}
			}
		}
	}
}

func noteUnavailableTools(body map[string]any, flat bool, names []string) {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	list := strings.Join(quoted, " or ")
	var note string
	if len(names) > 1 {
		note = "Do not call the " + list + " tools; they are placeholders that do nothing. Use the other tools you have been given instead."
	} else {
		note = "Do not call the " + list + " tool; it is a placeholder that does nothing. Use the other tools you have been given instead."
	}

	if flat {
		if instructions, ok := body["instructions"].(string); ok && strings.TrimSpace(instructions) != "" {
			body["instructions"] = instructions + "\n\n" + note
		} else {
			body["instructions"] = note
		}
		return
	}
	if _, ok := body["system"]; ok {
		return
	}
	messages, _ := body["messages"].([]any)
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok && first["role"] == "system" {
			if content, ok := first["content"].(string); ok {
				copied := cloneMap(first)
				copied["content"] = content + "\n\n" + note
				body["messages"] = append([]any{copied}, messages[1:]...)
				return
			}
		}
	}
	body["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, messages...)
}

// RestoreToolNames renames concealed tools back to the caller's spellings in
// Chat Completions deltas/JSON, Responses output and Claude content blocks.
func RestoreToolNames(payload any, rename map[string]string) any {
	if len(rename) == 0 || payload == nil {
		return payload
	}
	switch v := payload.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RestoreToolNames(item, rename)
		}
		return out
	case map[string]any:
		if v["type"] == "content_block_start" {
			if block, ok := v["content_block"].(map[string]any); ok && block["type"] == "tool_use" {
				if name, ok := block["name"].(string); ok {
					if original, ok := rename[name]; ok {
						copied := cloneMap(block)
						copied["name"] = original
						out := cloneMap(v)
						out["content_block"] = copied
						return out
					}
				}
			}
		}
		if content, ok := v["content"].([]any); ok {
			v["content"] = restoreBlocks(content, rename)
		}
		if choices, ok := v["choices"].([]any); ok {
			for _, raw := range choices {
				choice, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				for _, holder := range []string{"delta", "message"} {
					value, ok := choice[holder].(map[string]any)
					if !ok {
						continue
					}
					calls, ok := value["tool_calls"].([]any)
					if !ok {
						continue
					}
					for _, rawCall := range calls {
						call, ok := rawCall.(map[string]any)
						if !ok {
							continue
						}
						fn, ok := call["function"].(map[string]any)
						if !ok {
							continue
						}
						if name, ok := fn["name"].(string); ok {
							if original, ok := rename[name]; ok {
								fn["name"] = original
							}
						}
					}
				}
			}
		}
		if output, ok := v["output"].([]any); ok {
			for _, raw := range output {
				item, ok := raw.(map[string]any)
				if !ok || item["type"] != "function_call" {
					continue
				}
				if name, ok := item["name"].(string); ok {
					if original, ok := rename[name]; ok {
						item["name"] = original
					}
				}
			}
		}
		if response, ok := v["response"].(map[string]any); ok {
			RestoreToolNames(response, rename)
		}
		return v
	default:
		return payload
	}
}

func restoreBlocks(blocks []any, rename map[string]string) []any {
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		if name, ok := block["name"].(string); ok {
			if original, ok := rename[name]; ok {
				block["name"] = original
			}
		}
	}
	return blocks
}

func aliasesByOriginal(rename map[string]string) map[string]string {
	out := make(map[string]string, len(rename))
	for alias, original := range rename {
		out[original] = alias
	}
	return out
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// ApplyAnthropicFingerprint appends the same fingerprint tools in Anthropic
// (messages API) shape and conceals conflicting caller tools.
func ApplyAnthropicFingerprint(body map[string]any) map[string]string {
	rename := make(map[string]string)
	if body == nil {
		return rename
	}
	tools, _ := body["tools"].([]any)
	taken := make(map[string]bool, len(tools)+2)
	seen := make(map[string]bool, len(tools))
	kept := make([]any, 0, len(tools)+2)

	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		name, _ := tool["name"].(string)
		name = strings.TrimSpace(name)
		if name != "" {
			taken[name] = true
		}
		key := fingerprintKey(name)
		if key == "" {
			kept = append(kept, tool)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		def := fingerprintDefs[key]
		desc, _ := tool["description"].(string)
		if desc == def.Description && canonicalJSON(tool["input_schema"]) == canonicalJSON(def.Parameters) {
			kept = append(kept, tool)
			continue
		}
		alias := key + "_client"
		for i := 2; taken[alias]; i++ {
			alias = key + "_client" + itoa(i)
		}
		taken[alias] = true
		rename[alias] = key
		copied := cloneMap(tool)
		copied["name"] = alias
		kept = append(kept, copied)
	}
	appended := make([]string, 0, 2)
	for _, name := range fingerprintOrder {
		if seen[name] {
			continue
		}
		def := fingerprintDefs[name]
		kept = append(kept, map[string]any{
			"name":         name,
			"description":  def.Description,
			"input_schema": def.Parameters,
		})
		appended = append(appended, name)
	}
	body["tools"] = kept
	if len(appended) > 0 {
		note := "Do not call the `read` or `shell` tools; they are placeholders that do nothing. Use the other tools you have been given instead."
		if existing, ok := body["system"].(string); ok && strings.TrimSpace(existing) != "" {
			body["system"] = existing + "\n\n" + note
		} else if _, exists := body["system"]; !exists {
			body["system"] = note
		}
	}
	return rename
}
