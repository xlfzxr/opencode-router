package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
)

type Kind int

const (
	KindChat Kind = iota
	KindResponses
	KindMessages
)

func (k Kind) Path() string {
	switch k {
	case KindResponses:
		return "/inference/v1/responses"
	case KindMessages:
		return "/inference/v1/messages"
	default:
		return "/inference/v1/chat/completions"
	}
}

func (k Kind) String() string {
	switch k {
	case KindResponses:
		return "responses"
	case KindMessages:
		return "messages"
	default:
		return "chat"
	}
}

var museSparkRe = regexp.MustCompile(`(?i)^muse[-_]?spark(?:$|[-_:.\s])`)
var thinkingSuffixRe = regexp.MustCompile(`\([^()]+\)\s*$`)

func BaseModelID(model string) string {
	return strings.TrimSpace(thinkingSuffixRe.ReplaceAllString(strings.TrimSpace(model), ""))
}

func IsResponsesModel(model string) bool {
	base := BaseModelID(model)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return museSparkRe.MatchString(base)
}

func IsMessagesModel(model string) bool {
	return BaseModelID(model) == "union-alpha"
}

func KindFor(model string) Kind {
	if IsResponsesModel(model) {
		return KindResponses
	}
	if IsMessagesModel(model) {
		return KindMessages
	}
	return KindChat
}

type Model struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// fallbackModels is used when the upstream catalog is unreachable. Mirrors the
// free-tier members the router is expected to expose.
var fallbackModels = []string{
	"deepseek-v4.1-flash-free",
	"glm-5.3-flash-free",
	"nemotron-3.5-super-vl-free",
	"mimo-v2.6-flash-free",
	"minimax-m3.1-flash-free",
	"longcat-2.5-preview-free",
	"jev-1.13-free",
	"nemotron-3.5-lightning-free",
	"beam-free",
	"big-pickle",
}

func ParseCatalog(data []byte) []Model {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	var out []Model
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		out = append(out, Model{ID: id, Object: "model", OwnedBy: "opencode"})
	}
	if items, ok := payload["data"].([]any); ok {
		for _, raw := range items {
			switch item := raw.(type) {
			case string:
				add(item)
			case map[string]any:
				id, _ := item["id"].(string)
				add(id)
			}
		}
	}
	if len(out) == 0 {
		if items, ok := payload["models"].([]any); ok {
			for _, raw := range items {
				if item, ok := raw.(map[string]any); ok {
					id, _ := item["id"].(string)
					add(id)
				}
			}
		}
	}
	return out
}

func IsFreeModel(id string) bool {
	lower := strings.ToLower(strings.TrimSpace(id))
	if lower == "" {
		return false
	}
	// The catalog also advertises paid muse-spark ids (401 without a key) and
	// union-alpha (currently unavailable). Only truly free ids are exposed.
	return strings.Contains(lower, "free") || lower == "big-pickle" || strings.HasPrefix(lower, "beam-")
}

func FallbackCatalog() []Model {
	out := make([]Model, 0, len(fallbackModels))
	for _, id := range fallbackModels {
		out = append(out, Model{ID: id, Object: "model", OwnedBy: "opencode"})
	}
	return out
}
