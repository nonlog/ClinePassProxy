package translate

import (
	"encoding/json"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func mustConvertMessages(t *testing.T, body string, upstreamModel string, stream bool) map[string]any {
	t.Helper()
	out, err := ClaudeMessagesToChatCompletions([]byte(body), upstreamModel, stream)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	return out
}

func TestClaudeMessagesSystemAndTurns(t *testing.T) {
	out := mustConvertMessages(t, `{
		"model":"claude-sonnet-4.6",
		"max_tokens":4096,
		"temperature":0.4,
		"system":[{"type":"text","text":"You are terse."},{"type":"text","text":"x-anthropic-billing-header: abc"}],
		"messages":[
			{"role":"user","content":"hello"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"pondering"},{"type":"text","text":"hi"}]}
		]
	}`, "cline-upstream", true)

	if out["model"] != "cline-upstream" {
		t.Fatalf("model not mapped: %v", out["model"])
	}
	if out["stream"] != true {
		t.Fatalf("stream flag lost")
	}
	if out["temperature"] != 0.4 {
		t.Fatalf("temperature lost: %v", out["temperature"])
	}
	if number := models.Number(out["max_tokens"]); number != 4096 {
		t.Fatalf("max_tokens lost: %v", out["max_tokens"])
	}

	messages := models.List(out["messages"])
	if len(messages) != 3 {
		t.Fatalf("expected system + user + assistant, got %d", len(messages))
	}
	system := models.Object(messages[0])
	if models.String(system["role"]) != "system" || models.String(system["content"]) != "You are terse." {
		t.Fatalf("billing attribution was not stripped from the system prompt: %v", system)
	}
	if text := models.String(models.Object(messages[1])["content"]); text != "hello" {
		t.Fatalf("string user content changed: %v", messages[1])
	}
	assistant := models.Object(messages[2])
	assertSingleTextBlock(t, assistant["content"], "hi")
	if reasoning := models.String(assistant["reasoning_content"]); reasoning != "pondering" {
		t.Fatalf("unsigned assistant thinking must round-trip into reasoning_content, got %q", reasoning)
	}
}

func TestClaudeMessagesSignatureBlocksAreNotReplayed(t *testing.T) {
	out := mustConvertMessages(t, `{
		"model":"m",
		"messages":[{"role":"assistant","content":[
			{"type":"thinking","thinking":"signed","signature":"abc=="},
			{"type":"text","text":"visible"}
		]}]
	}`, "upstream", false)

	assistant := models.Object(models.List(out["messages"])[0])
	if _, present := assistant["reasoning_content"]; present {
		t.Fatalf("signed thinking from another provider must not be replayed: %v", assistant)
	}
	assertSingleTextBlock(t, assistant["content"], "visible")
}

func TestClaudeMessagesToolRoundTrip(t *testing.T) {
	out := mustConvertMessages(t, `{
		"model":"m",
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Oslo","units":"c","days":3}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"-1 C"}]},
				{"type":"text","text":"thanks"}
			]}
		],
		"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"get_weather"}
	}`, "upstream", false)

	messages := models.List(out["messages"])
	if len(messages) != 3 {
		t.Fatalf("expected assistant, tool, user messages, got %d", len(messages))
	}
	assistant := models.Object(messages[0])
	toolCalls := models.List(assistant["tool_calls"])
	if len(toolCalls) != 1 {
		t.Fatalf("tool_use did not become tool_calls: %v", assistant)
	}
	call := models.Object(toolCalls[0])
	if models.String(call["id"]) != "toolu_1" || models.String(models.Object(call["function"])["name"]) != "get_weather" {
		t.Fatalf("tool call identity changed: %v", call)
	}
	arguments := models.String(models.Object(call["function"])["arguments"])
	if arguments != `{"city":"Oslo","days":3,"units":"c"}` {
		t.Fatalf("tool arguments changed semantically, got %s", arguments)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v", err)
	}
	if parsed["city"] != "Oslo" || parsed["units"] != "c" || parsed["days"] != float64(3) {
		t.Fatalf("tool arguments lost content: %v", parsed)
	}

	tool := models.Object(messages[1])
	if models.String(tool["role"]) != "tool" || models.String(tool["tool_call_id"]) != "toolu_1" {
		t.Fatalf("tool_result did not become a tool message: %v", tool)
	}
	if models.String(tool["content"]) != "-1 C" {
		t.Fatalf("tool result content lost: %v", tool["content"])
	}
	if models.String(tool["name"]) != "get_weather" {
		t.Fatalf("tool result name must be restored: %v", tool["name"])
	}
	user := models.Object(messages[2])
	if models.String(user["role"]) != "user" || models.String(user["content"]) != "thanks" {
		t.Fatalf("trailing user content lost: %v", user)
	}

	tools := models.List(out["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools not converted: %v", out["tools"])
	}
	parameters := models.Object(models.Object(models.Object(tools[0])["function"])["parameters"])
	if models.String(parameters["type"]) != "object" {
		t.Fatalf("input_schema not mapped to parameters: %v", parameters)
	}
	choice := models.Object(out["tool_choice"])
	if models.String(models.Object(choice["function"])["name"]) != "get_weather" {
		t.Fatalf("tool_choice not converted: %v", out["tool_choice"])
	}
}

func TestClaudeMessagesThinkingBudget(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, "none"},
		{256, "minimal"},
		{1024, "low"},
		{4096, "medium"},
		{16000, "high"},
		{64000, "xhigh"},
	}
	for _, testCase := range cases {
		out := mustConvertMessages(t, `{"model":"m","thinking":{"type":"enabled","budget_tokens":`+itoa(testCase.budget)+`},"messages":[{"role":"user","content":"x"}]}`, "upstream", true)
		if got := models.String(out["reasoning_effort"]); got != testCase.want {
			t.Fatalf("budget %d mapped to %q, want %q", testCase.budget, got, testCase.want)
		}
	}
}

func TestClaudeMessagesPreservesWhitespaceOnlyTextBlocks(t *testing.T) {
	out := mustConvertMessages(t, `{
		"model":"m",
		"messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"\n\n"},{"type":"text","text":"b"}]}]
	}`, "upstream", false)
	content := models.List(models.Object(models.List(out["messages"])[0])["content"])
	if len(content) != 3 {
		t.Fatalf("whitespace-only blocks are part of the stable prompt prefix and must survive, got %d blocks", len(content))
	}
}

func TestClaudeMessagesImageSource(t *testing.T) {
	out := mustConvertMessages(t, `{
		"model":"m",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
		]}]
	}`, "upstream", false)
	content := models.List(models.Object(models.List(out["messages"])[0])["content"])
	part := models.Object(content[0])
	if models.String(part["type"]) != "image_url" {
		t.Fatalf("image block not converted: %v", part)
	}
	if url := models.String(models.Object(part["image_url"])["url"]); url != "data:image/png;base64,AAA" {
		t.Fatalf("image data URL changed: %s", url)
	}
}

func TestClaudeMessagesRejectsEmptyConversation(t *testing.T) {
	if _, err := ClaudeMessagesToChatCompletions([]byte(`{"model":"m","messages":[]}`), "upstream", false); err == nil {
		t.Fatal("expected an empty conversation to be rejected")
	}
}

func TestClaudeMessagesStableAcrossCalls(t *testing.T) {
	body := `{
		"model":"m","max_tokens":1024,"temperature":0.2,
		"system":"be brief",
		"messages":[
			{"role":"user","content":[{"type":"text","text":"one"}]},
			{"role":"assistant","content":[{"type":"text","text":"two"}]},
			{"role":"user","content":[{"type":"text","text":"three"}]}
		]
	}`
	first := mustConvertMessages(t, body, "upstream", true)
	second := mustConvertMessages(t, body, "upstream", true)
	firstJSON, _ := json.Marshal(first)
	secondJSON, _ := json.Marshal(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("conversion is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
}

func itoa(value int) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func assertSingleTextBlock(t *testing.T, value any, want string) {
	t.Helper()
	blocks := models.List(value)
	if len(blocks) != 1 {
		t.Fatalf("expected one content block, got %v", value)
	}
	block := models.Object(blocks[0])
	if models.String(block["type"]) != "text" || models.String(block["text"]) != want {
		t.Fatalf("text block changed: %v", block)
	}
}
