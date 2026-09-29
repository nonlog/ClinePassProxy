package translate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func eventTypes(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		for _, line := range strings.Split(string(frame), "\n") {
			if strings.HasPrefix(line, "event: ") {
				out = append(out, strings.TrimPrefix(line, "event: "))
			}
		}
	}
	return out
}

func TestClaudeStreamConverterPlainText(t *testing.T) {
	converter := NewClaudeStreamConverter("claude-sonnet-4.6", nil)
	start, err := converter.Start()
	if err != nil {
		t.Fatal(err)
	}
	if types := eventTypes(start); len(types) != 1 || types[0] != "message_start" {
		t.Fatalf("Start must emit message_start only, got %v", types)
	}

	var all [][]byte
	all = append(all, start...)
	for _, chunk := range []string{
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":100}}}`,
	} {
		frames, err := converter.Feed([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, frames...)
	}
	done, err := converter.Done()
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, done...)

	types := eventTypes(all)
	want := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if len(types) != len(want) {
		t.Fatalf("event sequence = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event %d = %q, want %q (%v)", i, types[i], want[i], types)
		}
	}

	joined := string(strings.Join(framesToStrings(all), ""))
	if !strings.Contains(joined, `"text":"Hello"`) || !strings.Contains(joined, `"text":" world"`) {
		t.Fatalf("text deltas were altered:\n%s", joined)
	}
	if !strings.Contains(joined, `"cache_read_input_tokens":100`) {
		t.Fatalf("cache read tokens lost from usage:\n%s", joined)
	}
	if !strings.Contains(joined, `"stop_reason":"end_turn"`) {
		t.Fatalf("stop reason not mapped:\n%s", joined)
	}
}

func TestClaudeStreamConverterReasoningThenToolCall(t *testing.T) {
	converter := NewClaudeStreamConverter("m", []byte(`{"tools":[{"name":"get_weather"}]}`))
	var all [][]byte
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"weighing options"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abc","function":{"name":"get_weather","arguments":"{\"city\""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":\"Oslo\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		frames, err := converter.Feed([]byte(chunk))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, frames...)
	}
	done, err := converter.Done()
	if err != nil {
		t.Fatal(err)
	}
	all = append(all, done...)

	joined := string(strings.Join(framesToStrings(all), ""))
	if !strings.Contains(joined, `"type":"thinking_delta"`) {
		t.Fatalf("reasoning not emitted as a thinking block:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"tool_use"`) || !strings.Contains(joined, `"name":"get_weather"`) {
		t.Fatalf("tool use block missing:\n%s", joined)
	}
	if !strings.Contains(joined, `"partial_json":"{\"city\":\"Oslo\"}"`) {
		t.Fatalf("tool arguments were not delivered in full:\n%s", joined)
	}
	if !strings.Contains(joined, `"stop_reason":"tool_use"`) {
		t.Fatalf("tool finish reason not mapped:\n%s", joined)
	}
}

func TestClaudeStreamConverterRestoresSanitizedToolName(t *testing.T) {
	original := []byte(`{
		"tools":[{"name":"My-Tool_Name"}],
		"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"My-Tool_Name","input":{}}]}]
	}`)
	converter := NewClaudeStreamConverter("m", original)
	frames, err := converter.Feed([]byte(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"mytoolname","arguments":"{}"}}]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	closing, err := converter.Feed([]byte(`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, closing...)
	done, err := converter.Done()
	if err != nil {
		t.Fatal(err)
	}
	frames = append(frames, done...)
	joined := string(strings.Join(framesToStrings(frames), ""))
	if !strings.Contains(joined, `"name":"My-Tool_Name"`) {
		t.Fatalf("declared tool name was not restored:\n%s", joined)
	}
}

func TestClaudeStreamConverterRefusesToCompleteTruncatedStream(t *testing.T) {
	// A stream that ends without a finish_reason did not complete. Synthesising
	// end_turn here would report a cut-off answer as a finished turn.
	converter := NewClaudeStreamConverter("m", nil)
	if _, err := converter.Feed([]byte(`{"choices":[{"index":0,"delta":{"content":"partial"}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := converter.Done(); !errors.Is(err, ErrUpstreamTruncated) {
		t.Fatalf("Done() = %v, want ErrUpstreamTruncated", err)
	}
	if converter.FinishReason() != "" {
		t.Fatalf("finish reason should still be empty, got %q", converter.FinishReason())
	}
	// The client still learns that the stream failed instead of ending silently.
	if frame := string(converter.ErrorFrame("truncated")); !strings.Contains(frame, "event: error") {
		t.Fatalf("error frame = %q", frame)
	}
}

func TestOpenAICompletionToClaudeNonStreaming(t *testing.T) {
	raw := `{
		"id":"chatcmpl-9","object":"chat.completion","model":"upstream",
		"choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"do_thing","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":50,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}
	}`
	converted, err := OpenAICompletionToClaude([]byte(raw), "claude-sonnet-4.6", nil)
	if err != nil {
		t.Fatal(err)
	}
	var message map[string]any
	if err := json.Unmarshal(converted, &message); err != nil {
		t.Fatal(err)
	}
	if message["stop_reason"] != "tool_use" {
		t.Fatalf("stop reason = %v", message["stop_reason"])
	}
	usage, _ := message["usage"].(map[string]any)
	if usage["cache_read_input_tokens"] != float64(40) {
		t.Fatalf("cache tokens lost: %v", usage)
	}
	content, _ := message["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("expected text + tool_use blocks, got %v", content)
	}
}

func framesToStrings(frames [][]byte) []string {
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		out = append(out, string(frame))
	}
	return out
}
