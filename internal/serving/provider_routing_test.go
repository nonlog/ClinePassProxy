package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestModelProviderSelectionPinsPlannerShape(t *testing.T) {
	var upstreamBody map[string]any
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"completion-1","model":"cline-pass/deepseek-v4.1-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer cline.Close()

	server, handler := newTestServer(t, cline.URL)
	settings := server.Config.Get()
	settings.Models = []models.Entry{{
		ID:               "deepseek-v4.1-flash",
		UpstreamID:       "cline-pass/deepseek-v4.1-flash",
		Providers:        []string{"runware"},
		ProviderPipeline: "planner",
	}}
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"deepseek-v4.1-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	options := models.Object(upstreamBody["providerOptions"])
	gateway := models.Object(options["gateway"])
	if got := models.List(gateway["only"]); len(got) != 1 || models.String(got[0]) != "runware" {
		t.Fatalf("providerOptions.gateway.only = %v", gateway["only"])
	}
	if upstreamBody["provider"] != nil {
		t.Fatalf("planner request unexpectedly sent direct provider shape: %#v", upstreamBody["provider"])
	}
}

func TestModelProviderSelectionRejectsSilentFallback(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"provider_metadata\":{\"gateway\":{\"routing\":{\"finalProvider\":\"openai-compatible-private\"}}}}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer cline.Close()

	server, handler := newTestServer(t, cline.URL)
	settings := server.Config.Get()
	settings.Models = []models.Entry{{ID: "m", UpstreamID: "cline-pass/m", Providers: []string{"alibaba"}, ProviderPipeline: "planner"}}
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream provider fallback returned status %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "configured provider selection was not honored") {
		t.Fatalf("provider mismatch was not surfaced: %s", recorder.Body.String())
	}
	if got := server.History.List(1, "")[0].Provider; got != "openai-compatible-private" {
		t.Fatalf("actual provider was not recorded: %q", got)
	}
}
