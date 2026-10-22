package routing

import "strings"

// hardCaps mirror 9router's combo.js: missing one drops or corrupts request
// data (e.g. an image is stripped). Soft caps only degrade a feature.
var hardCaps = map[string]bool{
	"vision":     true,
	"pdf":        true,
	"audioInput": true,
	"videoInput": true,
}

// capability is the modality/reasoning profile used for auto-switch ordering.
type capability map[string]bool

// modelCapabilities is a local table for the OpenCode free tier (the upstream
// catalog does not expose capability metadata). Values mirror 9router's
// providers/capabilities.js and models.dev.
var modelCapabilities = map[string]capability{
	"glm-5.3-flash":          {"vision": true, "videoInput": true, "pdf": true, "reasoning": true},
	"glm-5.3-flash:cloud":    {"vision": true, "videoInput": true, "pdf": true, "reasoning": true},
	"deepseek-v4.1-flash":    {"vision": true, "reasoning": true},
	"minimax-m3.1-flash":     {"vision": true, "reasoning": true},
	"minimax-m3":             {"vision": true, "reasoning": true},
	"nemotron-3.5-super-vl":  {"vision": true, "reasoning": true},
	"longcat-2.5-preview":    {"vision": true, "reasoning": true},
	"union-alpha":            {"vision": true},
	"space-bunny":            {"vision": true, "videoInput": true, "reasoning": true},
	"exo":                    {"vision": true, "reasoning": true},
	"fledge-alpha":           {"vision": true, "reasoning": true},
	"x-preview-f":            {"vision": true, "videoInput": true, "reasoning": true},
	"big-pickle":             {"reasoning": true},
	"nemotron-3.5-lightning": {"reasoning": true},
	"mimo-v2.6-flash":        {"reasoning": true},
	"flint-40b":              {"reasoning": true},
	"beam":                   {"reasoning": true},
	"jev-1.13":               {"reasoning": true},
}

func normalizeModelID(id string) string {
	m := strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	m = strings.TrimSuffix(m, ":cloud")
	m = strings.TrimSuffix(m, "-free")
	return m
}

// lookupCapabilities returns the capability profile for a model id, with a few
// pattern fallbacks for ids newer than the table.
func lookupCapabilities(model string) capability {
	id := normalizeModelID(model)
	if caps, ok := modelCapabilities[id]; ok {
		return caps
	}
	switch {
	case strings.Contains(id, "super-vl"), strings.Contains(id, "vl-"), strings.HasSuffix(id, "-vl"):
		return capability{"vision": true}
	case strings.Contains(id, "deepseek-v4"):
		return capability{"vision": true, "reasoning": true}
	case strings.Contains(id, "glm-5.3"):
		return capability{"vision": true, "videoInput": true, "pdf": true, "reasoning": true}
	case strings.Contains(id, "minimax-m3"):
		return capability{"vision": true, "reasoning": true}
	case strings.Contains(id, "vision"), strings.Contains(id, "-vl"):
		return capability{"vision": true}
	default:
		return capability{}
	}
}

// reorderByCapabilities ports 9router's combo.js reorder: stable sort by tier.
// Tier 0 satisfies all hard + soft capabilities, tier 1 all hard only, tier 2
// the rest. Never drops a model (fallback stays intact).
func reorderByCapabilities(models []string, required map[string]bool) []string {
	if len(required) == 0 || len(models) <= 1 {
		return models
	}
	hard := make([]string, 0, len(required))
	soft := make([]string, 0, len(required))
	for cap := range required {
		if hardCaps[cap] {
			hard = append(hard, cap)
		} else {
			soft = append(soft, cap)
		}
	}

	tierOf := func(model string) int {
		caps := lookupCapabilities(model)
		for _, c := range hard {
			if !caps[c] {
				return 2
			}
		}
		for _, c := range soft {
			if !caps[c] {
				return 1
			}
		}
		return 0
	}

	out := append([]string(nil), models...)
	// Insertion sort by tier only: stable, keeps relative order inside tiers.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && tierOf(out[j]) < tierOf(out[j-1]); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// detectRequiredCapabilities ports 9router's combo.js detection. Modalities are
// scanned only on the current (trailing) user turn; returns a set of "vision",
// "pdf", "audioInput", "videoInput".
func detectRequiredCapabilities(body map[string]any) map[string]bool {
	required := map[string]bool{}
	if body == nil {
		return required
	}

	addByMime := func(mime string) {
		if mime == "" {
			return
		}
		switch {
		case strings.HasPrefix(mime, "image/"):
			required["vision"] = true
		case mime == "application/pdf":
			required["pdf"] = true
		case strings.HasPrefix(mime, "audio/"):
			required["audioInput"] = true
		case strings.HasPrefix(mime, "video/"):
			required["videoInput"] = true
		}
	}
	dataURIMime := func(s string) string {
		if !strings.HasPrefix(s, "data:") {
			return ""
		}
		rest := s[len("data:"):]
		if i := strings.IndexAny(rest, ";,"); i >= 0 {
			return rest[:i]
		}
		return ""
	}

	var scanBlock func(b map[string]any)
	scanBlock = func(b map[string]any) {
		if b == nil {
			return
		}
		typ, _ := b["type"].(string)
		switch typ {
		case "image_url", "image", "input_image":
			required["vision"] = true
		case "input_audio", "audio_url", "audio":
			required["audioInput"] = true
		case "input_video", "video_url", "video":
			required["videoInput"] = true
		case "file", "document", "input_file":
			mime := ""
			if audio, ok := b["input_audio"].(map[string]any); ok {
				if format, ok := audio["format"].(string); ok {
					mime = "audio/" + format
				}
			}
			if mime == "" {
				if file, ok := b["file"].(map[string]any); ok {
					if data, ok := file["file_data"].(string); ok {
						mime = dataURIMime(data)
					}
				}
			}
			if mime == "" {
				if source, ok := b["source"].(map[string]any); ok {
					if mt, ok := source["media_type"].(string); ok {
						mime = mt
					} else if data, ok := source["data"].(string); ok {
						mime = dataURIMime(data)
					}
				}
			}
			if mime != "" {
				addByMime(mime)
			} else {
				required["pdf"] = true
			}
		}
		if inline, ok := b["inlineData"].(map[string]any); ok {
			if mt, ok := inline["mimeType"].(string); ok {
				addByMime(mt)
			}
		}
		if fileData, ok := b["fileData"].(map[string]any); ok {
			if mt, ok := fileData["mimeType"].(string); ok {
				addByMime(mt)
			}
		}
	}

	scanContent := func(content any) {
		blocks, ok := content.([]any)
		if !ok {
			return
		}
		for _, raw := range blocks {
			if b, ok := raw.(map[string]any); ok {
				scanBlock(b)
			}
		}
	}

	scanMessage := func(m map[string]any) {
		if m == nil {
			return
		}
		if images, ok := m["images"].([]any); ok && len(images) > 0 {
			required["vision"] = true
		}
		for _, key := range []string{"experimental_attachments", "attachments"} {
			if atts, ok := m[key].([]any); ok {
				for _, raw := range atts {
					att, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					mime := ""
					if ct, ok := att["contentType"].(string); ok {
						mime = ct
					} else if mt, ok := att["mediaType"].(string); ok {
						mime = mt
					} else if u, ok := att["url"].(string); ok {
						mime = dataURIMime(u)
					}
					if mime != "" {
						addByMime(mime)
					} else if att["url"] != nil || att["data"] != nil {
						required["vision"] = true
					}
				}
			}
		}
		if m["image_url"] != nil || m["image"] != nil {
			required["vision"] = true
		}
		if m["audio_url"] != nil || m["audio"] != nil {
			required["audioInput"] = true
		}
		scanContent(m["content"])
		if s, ok := m["content"].(string); ok {
			switch {
			case strings.Contains(s, "data:image/"):
				required["vision"] = true
			case strings.Contains(s, "data:audio/"):
				required["audioInput"] = true
			case strings.Contains(s, "data:application/pdf"):
				required["pdf"] = true
			}
		}
	}

	// OpenAI/Claude/Hermes/Ollama shapes.
	for _, raw := range trailingUserItems(body["messages"]) {
		if m, ok := raw.(map[string]any); ok {
			scanMessage(m)
		}
	}
	// OpenAI Responses shape.
	for _, raw := range trailingUserItems(body["input"]) {
		if item, ok := raw.(map[string]any); ok {
			scanContent(item["content"])
		}
	}
	// Gemini / Antigravity shape.
	contents := body["contents"]
	if contents == nil {
		if req, ok := body["request"].(map[string]any); ok {
			contents = req["contents"]
		}
	}
	for _, raw := range trailingUserItems(contents) {
		if c, ok := raw.(map[string]any); ok {
			scanContent(c["parts"])
		}
	}
	return required
}

// trailingUserItems returns the items after the last assistant turn, mirroring
// 9router's helper of the same name.
func trailingUserItems(v any) []any {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	isAssistant := func(raw any) bool {
		m, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		role, _ := m["role"].(string)
		return role == "assistant" || role == "model"
	}
	i := len(arr) - 1
	for i >= 0 && !isAssistant(arr[i]) {
		i--
	}
	return arr[i+1:]
}

// ZeroContentChat ports 9router's isZeroContentResponse for aggregated chat
// completions: true when a 2xx response carries no usable content, so combo
// fallback can continue to the next model.
func ZeroContentChat(data map[string]any) bool {
	if data == nil {
		return false
	}
	choices, _ := data["choices"].([]any)
	if len(choices) == 0 {
		return true
	}
	choice, _ := choices[0].(map[string]any)
	if choice == nil {
		return true
	}
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		msg, _ = choice["delta"].(map[string]any)
	}

	hasContent := false
	hasTools := false
	hasReasoning := false
	if msg != nil {
		if s, ok := msg["content"].(string); ok && strings.TrimSpace(s) != "" {
			hasContent = true
		}
		if arr, ok := msg["content"].([]any); ok && len(arr) > 0 {
			hasContent = true
		}
		if tc, ok := msg["tool_calls"].([]any); ok && len(tc) > 0 {
			hasTools = true
		}
		if msg["function_call"] != nil {
			hasTools = true
		}
		if rc, ok := msg["reasoning_content"].(string); ok && strings.TrimSpace(rc) != "" {
			hasReasoning = true
		}
	}

	finish, _ := choice["finish_reason"].(string)
	isNormalFinish := finish == "stop" || finish == "length" || finish == "tool_calls" || finish == "function_call"
	return !hasContent && !hasTools && !hasReasoning &&
		(!isNormalFinish || finish == "in_progress" || finish == "incomplete")
}
