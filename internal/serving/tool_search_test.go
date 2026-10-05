package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// Exercise the public route, not just a converter: discovery returns a native
// search call, its output loads a namespaced MCP function, and the next request
// can invoke that function with the original client identity restored.
func TestResponsesClientToolSearchHTTPRoundTrip(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			var calls atomic.Int32
			cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				tools := models.List(body["tools"])
				name, args, id := "tool_search", `{"query":"probe ping"}`, "search1"
				if call == 1 {
					if len(tools) != 1 || models.String(models.Object(models.Object(tools[0])["function"])["name"]) != name {
						t.Errorf("discovery absent upstream: %#v", body)
					}
				} else {
					name, args, id = "mcp__probe__ping", "{}", "ping1"
					if len(tools) != 2 || models.String(models.Object(models.Object(tools[1])["function"])["name"]) != name {
						t.Errorf("loaded function absent upstream: %#v", body)
					}
					messages := models.List(body["messages"])
					if len(messages) != 3 || models.String(models.Object(messages[2])["tool_call_id"]) != "search1" {
						t.Errorf("search history lost correlation/order: %#v", messages)
					}
				}
				tool := map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}
				if models.Bool(body["stream"]) {
					w.Header().Set("Content-Type", "text/event-stream")
					tool["index"] = 0
					chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{tool}}}}}
					raw, _ := json.Marshal(chunk)
					_, _ = fmt.Fprintf(w, "data: %s\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", raw)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"tool_calls": []any{tool}}, "finish_reason": "tool_calls"}}})
				}
			}))
			defer cline.Close()
			_, handler := newTestServer(t, cline.URL)
			requestBody := map[string]any{"model": "m", "stream": streaming, "input": "find ping", "tools": []any{map[string]any{"type": "tool_search", "execution": "client", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}}}}
			for turn := 0; turn < 2; turn++ {
				raw, _ := json.Marshal(requestBody)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(raw)))
				request.Header.Set("Authorization", "Bearer gateway-key")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusOK {
					t.Fatalf("turn %d status=%d body=%s", turn, recorder.Code, recorder.Body.String())
				}
				var response map[string]any
				if streaming {
					for _, frame := range strings.Split(recorder.Body.String(), "\n\n") {
						if _, data, ok := strings.Cut(frame, "\ndata: "); ok {
							event, _ := models.DecodeObject([]byte(data))
							if models.String(event["type"]) == "response.completed" {
								response = models.Object(event["response"])
							}
						}
					}
				} else {
					response, _ = models.DecodeObject(recorder.Body.Bytes())
				}
				output := models.List(response["output"])
				if len(output) != 1 {
					t.Fatalf("turn %d missing output: %s", turn, recorder.Body.String())
				}
				item := models.Object(output[0])
				if turn == 0 {
					if item["type"] != "tool_search_call" || item["call_id"] != "search1" || item["execution"] != "client" {
						t.Fatalf("not a native client search call: %#v", item)
					}
					loaded := map[string]any{"type": "namespace", "name": "mcp__probe", "tools": []any{map[string]any{"type": "function", "name": "ping", "defer_loading": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}}
					requestBody["input"] = []any{map[string]any{"role": "user", "content": "find ping"}, item, map[string]any{"type": "tool_search_output", "execution": "client", "status": "completed", "call_id": "search1", "tools": []any{loaded}}}
				} else if item["type"] != "function_call" || item["namespace"] != "mcp__probe" || item["name"] != "ping" || item["call_id"] != "ping1" {
					t.Fatalf("loaded MCP identity lost: %#v", item)
				}
			}
			if count := calls.Load(); count != 2 {
				t.Fatalf("unexpected upstream calls=%d", count)
			}
		})
	}
}
