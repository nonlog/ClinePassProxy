package serving

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func nativeSearchRequest(stream bool) string {
	body := map[string]any{
		"model":      "claude-sonnet-4.6",
		"max_tokens": 128,
		"stream":     stream,
		"tools": []any{map[string]any{
			"type":     "web_search_20250305",
			"name":     "web_search",
			"max_uses": 5,
		}},
		"messages": []any{map[string]any{"role": "user", "content": "Find one result"}},
	}
	raw, _ := json.Marshal(body)
	return string(raw)
}

func nativeSearchResponse() []byte {
	body := map[string]any{
		"id":    "msg_native-search",
		"type":  "message",
		"role":  "assistant",
		"model": "claude-sonnet-4.6",
		"content": []any{
			map[string]any{"type": "server_tool_use", "id": "srv_1", "name": "web_search", "input": map[string]any{"query": "Find one result"}},
			map[string]any{"type": "web_search_tool_result", "tool_use_id": "srv_1", "content": []any{map[string]any{"type": "web_search_result", "title": "Example", "url": "https://example.com"}}},
			map[string]any{"type": "text", "text": "Found one result."},
		},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 4},
	}
	raw, _ := json.Marshal(body)
	return raw
}

func TestHasNativeClaudeWebSearchDoesNotInterceptClientTools(t *testing.T) {
	if !hasNativeClaudeWebSearch(map[string]any{"tools": []any{map[string]any{
		"type": "web_search_20250305", "name": "web_search",
	}}}) {
		t.Fatal("versioned Anthropic web search tool was not detected")
	}
	if hasNativeClaudeWebSearch(map[string]any{"tools": []any{map[string]any{
		"type": "function", "name": "WebSearch",
	}}}) {
		t.Fatal("ordinary client WebSearch tool was intercepted")
	}
}

func TestNativeClaudeSearchForwardsStreamingRequestToCommandCodeProxy(t *testing.T) {
	clineCalled := false
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		clineCalled = true
		http.Error(w, "Cline should not receive native search", http.StatusBadGateway)
	}))
	defer cline.Close()

	ccp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer ccp-key" {
			t.Fatalf("authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("request JSON: %v", err)
		}
		if request["stream"] != true {
			t.Fatalf("stream flag = %v", request["stream"])
		}
		tools, _ := request["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search_20250305" {
			t.Fatalf("native search tool was not preserved: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native-search\"}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"srv_1\",\"content\":[{\"type\":\"web_search_result\",\"title\":\"Example\",\"url\":\"https://example.com\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
		_, _ = w.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
		_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer ccp.Close()

	server, handler := newTestServer(t, cline.URL)
	settings := server.Config.Get()
	settings.SearchBaseURL = ccp.URL
	settings.SearchAPIKey = "ccp-key"
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(nativeSearchRequest(true)))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "web_search_tool_result") {
		t.Fatalf("native search result missing: %s", recorder.Body.String())
	}
	if clineCalled {
		t.Fatal("Cline received a native search request")
	}
}

func TestNativeClaudeSearchForwardsNonStreamingRequestToCommandCodeProxy(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Cline should not receive native search", http.StatusBadGateway)
	}))
	defer cline.Close()
	ccp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" || r.Header.Get("Authorization") != "Bearer ccp-key" {
			t.Fatalf("unexpected CCP request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(nativeSearchResponse())
	}))
	defer ccp.Close()

	server, handler := newTestServer(t, cline.URL)
	settings := server.Config.Get()
	settings.SearchBaseURL = ccp.URL
	settings.SearchAPIKey = "ccp-key"
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(nativeSearchRequest(false)))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "web_search_tool_result") {
		t.Fatalf("native search result missing: %s", recorder.Body.String())
	}
}

func TestNativeClaudeSearchReportsTruncatedUpstreamStream(t *testing.T) {
	ccp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
	}))
	defer ccp.Close()

	server, handler := newTestServer(t, "http://cline.invalid")
	settings := server.Config.Get()
	settings.SearchBaseURL = ccp.URL
	settings.SearchAPIKey = "ccp-key"
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(nativeSearchRequest(true)))
	request.Header.Set("Authorization", "Bearer gateway-key")
	handler.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("truncated search stream did not emit an error event: %s", body)
	}
	if strings.Contains(body, "event: message_stop") {
		t.Fatalf("truncated search stream was reported as complete: %s", body)
	}
}
