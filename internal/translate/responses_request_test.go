package translate

import (
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestResponsesToChatCompletionsLifecycle(t *testing.T) {
	out, err := ResponsesToChatCompletions([]byte(`{
		"model":"gpt-5.6-terra",
		"stream":true,
		"instructions":"be precise",
		"max_output_tokens":2048,
		"reasoning":{"effort":"high"},
		"prompt_cache_key":"session-1",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Oslo\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"-1 C"}
		],
		"tools":[
			{"type":"function","name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}},
			{"type":"namespace","name":"mcp__files","tools":[{"type":"function","name":"read","parameters":{"type":"object","properties":{}}}]},
			{"type":"custom","name":"shell","description":"raw shell"},
			{"type":"web_search"}
		],
		"tool_choice":{"type":"function","name":"get_weather"}
	}`), "cline-upstream", true)
	if err != nil {
		t.Fatal(err)
	}

	if out["model"] != "cline-upstream" || out["stream"] != true {
		t.Fatalf("model/stream not set: %v %v", out["model"], out["stream"])
	}
	if got := models.Number(out["max_tokens"]); got != 2048 {
		t.Fatalf("max_output_tokens not mapped: %v", got)
	}
	if out["reasoning_effort"] != "high" {
		t.Fatalf("reasoning effort not mapped: %v", out["reasoning_effort"])
	}

	messages := models.List(out["messages"])
	if len(messages) != 4 {
		t.Fatalf("expected system + user + assistant tool_call + tool output, got %d: %v", len(messages), messages)
	}
	if models.String(models.Object(messages[0])["content"]) != "be precise" {
		t.Fatalf("instructions not converted to a system message: %v", messages[0])
	}
	assistant := models.Object(messages[2])
	if models.String(assistant["role"]) != "assistant" {
		t.Fatalf("function_call did not become an assistant tool call: %v", assistant)
	}
	tool := models.Object(messages[3])
	if models.String(tool["role"]) != "tool" || models.String(tool["tool_call_id"]) != "call_1" {
		t.Fatalf("function_call_output did not become a tool message: %v", tool)
	}
	if models.String(tool["content"]) != "-1 C" {
		t.Fatalf("tool output content lost: %v", tool)
	}

	tools := models.List(out["tools"])
	if len(tools) != 3 {
		t.Fatalf("expected three Chat Completions tools (function, namespaced, custom), got %d: %v", len(tools), tools)
	}
	names := []string{}
	for _, raw := range tools {
		names = append(names, models.String(models.Object(models.Object(raw)["function"])["name"]))
	}
	expected := map[string]bool{"get_weather": true, "mcp__files__read": true, "shell": true}
	for _, name := range names {
		if !expected[name] {
			t.Fatalf("unexpected tool name %q in %v", name, names)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		t.Fatalf("missing tools: %v (got %v)", expected, names)
	}
	choice := models.Object(out["tool_choice"])
	if models.String(models.Object(choice["function"])["name"]) != "get_weather" {
		t.Fatalf("tool_choice not converted: %v", out["tool_choice"])
	}
}

func TestResponsesToChatCompletionsStringInput(t *testing.T) {
	out, err := ResponsesToChatCompletions([]byte(`{"model":"m","input":"hello"}`), "upstream", false)
	if err != nil {
		t.Fatal(err)
	}
	messages := models.List(out["messages"])
	if len(messages) != 1 {
		t.Fatalf("expected one message, got %v", messages)
	}
	if models.String(models.Object(messages[0])["content"]) != "hello" {
		t.Fatalf("string input lost: %v", messages[0])
	}
}

func TestResponsesToChatCompletionsStructuredFormat(t *testing.T) {
	out, err := ResponsesToChatCompletions([]byte(`{
		"model":"m",
		"input":"x",
		"text":{"format":{"type":"json_schema","name":"Out","strict":true,"schema":{"type":"object","properties":{"a":{"type":"string"}}}}}
	}`), "upstream", false)
	if err != nil {
		t.Fatal(err)
	}
	format := models.Object(out["response_format"])
	if models.String(format["type"]) != "json_schema" {
		t.Fatalf("structured output format not mapped: %v", format)
	}
	schema := models.Object(models.Object(format["json_schema"])["schema"])
	if models.String(schema["type"]) != "object" {
		t.Fatalf("schema lost: %v", format)
	}
}

func TestResponsesToChatCompletionsRejectsEmptyInput(t *testing.T) {
	if _, err := ResponsesToChatCompletions([]byte(`{"model":"m","input":[]}`), "upstream", false); err == nil {
		t.Fatal("expected an empty input to be rejected")
	}
}
