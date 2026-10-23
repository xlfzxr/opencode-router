package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestMessagesToolRoundtrip(t *testing.T) {
	body := decode(t, `{"model":"ocr","max_tokens":100,"system":[{"type":"text","text":"system"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read","input":{"path":"x"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"},{"type":"text","text":"continue"}]}],"tools":[{"name":"read","input_schema":{"type":"object"}}],"tool_choice":{"type":"any","disable_parallel_tool_use":true}}`)
	out, err := MessagesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	messages := out["messages"].([]any)
	if len(messages) != 4 || messages[2].(map[string]any)["role"] != "tool" || out["tool_choice"] != "required" || out["parallel_tool_calls"] != false {
		t.Fatalf("conversion: %+v", out)
	}
	reply := decode(t, `{"id":"x","model":"good-free","choices":[{"message":{"content":"hello","tool_calls":[{"id":"call_1","function":{"name":"read","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":4}}`)
	result, err := ChatToMessage(reply)
	if err != nil {
		t.Fatal(err)
	}
	if result["stop_reason"] != "tool_use" || len(result["content"].([]any)) != 2 {
		t.Fatalf("reply: %+v", result)
	}
}
func TestRejectUnsupportedContent(t *testing.T) {
	for _, payload := range []string{`{"max_tokens":1,"messages":[{"role":"user","content":[{"type":"document"}]}]}`, `{"max_tokens":0,"messages":[]}`, `{"messages":[{"role":"user","content":"hi"}]}`} {
		if _, err := MessagesToChat(decode(t, payload)); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}
func TestMessageStreamTextToolsAndUsage(t *testing.T) {
	var out bytes.Buffer
	stream := &MessageStream{W: &out, Model: "ocr"}
	frames := []string{
		`{"id":"m","model":"good-free","choices":[{"delta":{"content":"hello"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read","arguments":"{\"path\":"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3}}`, "[DONE]",
	}
	for _, frame := range frames {
		data := "data: " + frame + "\n\n"
		for _, b := range []byte(data) {
			if _, err := stream.Write([]byte{b}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := stream.Finish(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"event: message_start", "event: content_block_start", "input_json_delta", "event: message_stop", `"stop_reason":"tool_use"`, `"input_tokens":5`, `"output_tokens":3`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %s in %s", want, &out)
		}
	}
	if strings.Count(out.String(), "event: content_block_stop") != 2 {
		t.Fatal(out.String())
	}
	if err := (&MessageStream{}).Finish(); err == nil {
		t.Fatal("truncated stream succeeded")
	}
}
func TestModelsDevFreeProviderFiltering(t *testing.T) {
	data := []byte(`{"opencode":{"models":{"a":{"cost":{"input":0,"output":0}},"paid":{"cost":{"input":1,"output":0}},"unknown":{},"partial":{"cost":{"input":0}}}},"other":{"models":{"foreign":{"cost":{"input":0,"output":0}}}}}`)
	models, err := ParseModelsDev(data, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "a" {
		t.Fatalf("models: %+v", models)
	}
	if _, err := ParseModelsDev(data, "missing"); err == nil {
		t.Fatal("missing provider succeeded")
	}
}
