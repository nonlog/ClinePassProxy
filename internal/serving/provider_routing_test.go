package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestModelProviderSelectionPinsBothClineRoutingShapes(t *testing.T) {
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
		ID:         "deepseek-v4.1-flash",
		UpstreamID: "cline-pass/deepseek-v4.1-flash",
		Providers:  []string{"runware"},
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
	provider := models.Object(upstreamBody["provider"])
	if got := models.List(provider["only"]); len(got) != 1 || models.String(got[0]) != "runware" {
		t.Fatalf("provider.only = %v", provider["only"])
	}
}
