package translate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

const clientSearchRequest = `{"model":"m","input":"Find the probe ping tool","tools":[{"type":"tool_search","execution":"client","description":"Discover MCP tools","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}]}`

func searchTranslation(t *testing.T, original []byte) (map[string]any, []byte) {
	t.Helper()
	payload, err := ResponsesToChatCompletions(original, "upstream-model", true)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return payload, encoded
}

func TestResponsesClientToolSearchDeclaration(t *testing.T) {
	payload, _ := searchTranslation(t, []byte(clientSearchRequest))
	tools := models.List(payload["tools"])
	if len(tools) != 1 {
		t.Fatalf("client discovery tool was lost: %#v", payload)
	}
	function := models.Object(models.Object(tools[0])["function"])
	if models.String(function["name"]) != "tool_search" || models.String(function["description"]) != "Discover MCP tools" {
		t.Fatalf("discovery identity lost: %#v", function)
	}
	if !reflect.DeepEqual(models.List(models.Object(function["parameters"])["required"]), []any{"query"}) {
		t.Fatalf("client search schema lost: %#v", function)
	}
}

func TestResponsesClientToolSearchRoundTrip(t *testing.T) {
	original := []byte(clientSearchRequest)
	_, translated := searchTranslation(t, original)
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var response map[string]any
			if !streaming {
				raw := []byte(`{"id":"chat","choices":[{"message":{"tool_calls":[{"id":"call_search","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"probe ping\"}"}}]},"finish_reason":"tool_calls"}]}`)
				converted, err := OpenAICompletionToResponses(raw, "m", original, translated)
				if err != nil {
					t.Fatal(err)
				}
				response, _ = models.DecodeObject(converted)
			} else {
				converter := NewResponsesStreamConverter("m", original, translated)
				var frames [][]byte
				for _, chunk := range []string{
					`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_search"}]}}]}`,
					`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"tool_search","arguments":"{\"query\":"}}]}}]}`,
					`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"probe ping\"}"}}]},"finish_reason":"tool_calls"}]}`,
				} {
					out, err := converter.Feed([]byte(chunk))
					if err != nil {
						t.Fatal(err)
					}
					frames = append(frames, out...)
				}
				out, err := converter.Done()
				if err != nil {
					t.Fatal(err)
				}
				frames = append(frames, out...)
				added, done := 0, 0
				for _, frame := range frames {
					_, data, _ := strings.Cut(string(frame), "\ndata: ")
					event, err := models.DecodeObject([]byte(strings.TrimSpace(data)))
					if err != nil {
						t.Fatal(err)
					}
					kind := models.String(event["type"])
					if strings.HasPrefix(kind, "response.function_call_arguments.") {
						t.Fatalf("native search must not masquerade as function call: %s", frame)
					}
					if kind == "response.output_item.added" || kind == "response.output_item.done" {
						item := models.Object(event["item"])
						if models.String(item["type"]) != "tool_search_call" || models.String(item["execution"]) != "client" {
							t.Fatalf("not a native discovery item: %s", frame)
						}
						if kind == "response.output_item.added" {
							added++
						} else {
							done++
						}
					}
					if kind == "response.completed" {
						response = models.Object(event["response"])
					}
				}
				if added != 1 || done != 1 {
					t.Fatalf("search lifecycle added/done=%d/%d", added, done)
				}
			}
			items := models.List(response["output"])
			if len(items) != 1 {
				t.Fatalf("missing discovery output: %#v", response)
			}
			item := models.Object(items[0])
			if models.String(item["type"]) != "tool_search_call" || models.String(item["execution"]) != "client" || models.String(item["call_id"]) != "call_search" {
				t.Fatalf("discovery call changed type or correlation: %#v", item)
			}
			if models.String(models.Object(item["arguments"])["query"]) != "probe ping" {
				t.Fatalf("native search arguments must be an object: %#v", item)
			}
		})
	}
}

func TestResponsesRejectUnsupportedToolSearch(t *testing.T) {
	for _, tool := range []string{`{"type":"tool_search"}`, `{"type":"tool_search","execution":"server"}`, `{"type":"tool_search","execution":"client"}`} {
		_, err := ResponsesToChatCompletions([]byte(`{"input":"find tools","tools":[`+tool+`]}`), "m", true)
		if err == nil || !strings.Contains(err.Error(), "tool_search") {
			t.Fatalf("unsupported/invalid discovery must fail explicitly, not disappear: %s, err=%v", tool, err)
		}
	}
}

func TestResponsesClientToolSearchRejectsInvalidArguments(t *testing.T) {
	original := []byte(clientSearchRequest)
	_, translated := searchTranslation(t, original)
	for _, arguments := range []string{`{"query":`, `"not an object"`} {
		completion := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"id": "search", "function": map[string]any{"name": "tool_search", "arguments": arguments}}}}, "finish_reason": "tool_calls"}}}
		raw, _ := json.Marshal(completion)
		if _, err := OpenAICompletionToResponses(raw, "m", original, translated); err == nil {
			t.Fatalf("invalid discovery arguments accepted: %s", arguments)
		}
		completion["choices"] = []any{map[string]any{"delta": models.Object(models.List(completion["choices"])[0])["message"], "finish_reason": "tool_calls"}}
		raw, _ = json.Marshal(completion)
		if _, err := NewResponsesStreamConverter("m", original, translated).Feed(raw); err == nil {
			t.Fatalf("invalid streaming discovery arguments accepted: %s", arguments)
		}
	}
}
