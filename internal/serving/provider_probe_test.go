package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestProviderProbeResults(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantPipe string
		want     []string
	}{
		{
			name:     "planner message",
			body:     `{"error":{"message":"Available providers are: alibaba, runware."}}`,
			wantPipe: "planner",
			want:     []string{"alibaba", "runware"},
		},
		{
			name:     "direct metadata",
			body:     `{"error":{"metadata":{"available_providers":["runware","deepseek"]}}}`,
			wantPipe: "direct",
			want:     []string{"runware", "deepseek"},
		},
		{
			name:     "planner error string",
			body:     `{"error":"inference request failed. Available providers are: alibaba, runware"}`,
			wantPipe: "planner",
			want:     []string{"alibaba", "runware"},
		},
		{
			name:     "direct metadata embedded in error string",
			body:     `{"error":"inference request failed: {\"error\":{\"metadata\":{\"available_providers\":[\"open-inference\",\"relace\",\"deepinfra\"]}}}"}`,
			wantPipe: "direct",
			want:     []string{"open-inference", "relace", "deepinfra"},
		},
		{
			name:     "planner JSON embedded in error string",
			body:     `{"error":"Cline Vercel failed: {\"error\":{\"message\":\"Available providers are: alibaba, deepseek, runware\",\"type\":\"invalid_request_error\",\"param\":{\"modelId\":\"deepseek/deepseek-v4.1-flash\"}}}"}`,
			wantPipe: "planner",
			want:     []string{"alibaba", "deepseek", "runware"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			providers, pipeline := providerProbeResults([]byte(test.body))
			if pipeline != test.wantPipe {
				t.Fatalf("pipeline = %q, want %q", pipeline, test.wantPipe)
			}
			if strings.Join(providers, ",") != strings.Join(test.want, ",") {
				t.Fatalf("providers = %#v, want %#v", providers, test.want)
			}
		})
	}
}

func TestProviderProbeResultsReadsStreamingFinalProvider(t *testing.T) {
	body := []byte("data: {\"choices\":[{\"delta\":{\"provider_metadata\":{\"gateway\":{\"routing\":{\"finalProvider\":\"alibaba\"}}}}}]}\n\ndata: [DONE]\n\n")
	providers, pipeline := providerProbeResults(body)
	if pipeline != "planner" {
		t.Fatalf("pipeline = %q, want planner", pipeline)
	}
	if len(providers) != 0 {
		t.Fatalf("successful stream provider must not be treated as a catalog: %#v", providers)
	}
}

func TestProbeModelProvidersSendsPlannerShape(t *testing.T) {
	calls := 0
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			return
		}
		if calls == 1 {
			if body["provider"] != nil || body["providerOptions"] != nil {
				t.Errorf("pipeline identification unexpectedly sent provider fields: %#v", body)
			}
			if models.Number(body["max_tokens"]) != 64 || models.String(models.Object(models.List(body["messages"])[0])["content"]) != "Reply with the single word: ready" {
				t.Errorf("pipeline identification budget/prompt = %#v/%#v", body["max_tokens"], body["messages"])
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"provider_metadata":{"gateway":{"routing":{"finalProvider":"alibaba"}}}}}]}`))
			return
		}
		gateway := modelsObject(modelsObject(body["providerOptions"])["gateway"])
		if got := stringList(gateway["only"]); len(got) != 1 || got[0] != "__probe__" {
			t.Errorf("gateway.only = %#v", got)
		}
		if body["provider"] != nil {
			t.Errorf("planner probe unexpectedly sent provider.only: %#v", body["provider"])
		}
		if models.Bool(body["stream"]) || models.Number(body["max_tokens"]) != 16 {
			t.Errorf("probe budget/stream = %#v/%#v", body["max_tokens"], body["stream"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Available providers are: alibaba, runware."}}`))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/models/providers/probe", strings.NewReader(`{"model":"cline-pass/demo","upstream_model":"deepseek-v4.1-flash"}`))
	req.Header.Set("Authorization", "Bearer management-token")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		OK        bool `json:"ok"`
		Status    int  `json:"status"`
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || !response.OK || response.Status != http.StatusBadRequest || len(response.Providers) != 2 || response.Providers[0].Name != "alibaba" {
		t.Fatalf("probe response = %#v", response)
	}
}

func TestProbeModelProvidersFallsBackToDirectShape(t *testing.T) {
	calls := 0
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			return
		}
		if calls == 1 {
			if body["provider"] != nil || body["providerOptions"] != nil {
				t.Errorf("pipeline identification unexpectedly sent provider fields: %#v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"provider":"GMICloud","model":"deepseek/deepseek-v4.1-flash","choices":[{"message":{"content":"OK"}}]}`))
			return
		}
		if body["providerOptions"] != nil {
			t.Errorf("direct probe sent planner providerOptions: %#v", body["providerOptions"])
		}
		provider := modelsObject(body["provider"])
		if got := stringList(provider["only"]); len(got) != 1 || got[0] != "__probe__" {
			t.Errorf("provider.only = %#v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Openrouter returned HTTP 404: {\"error\":{\"metadata\":{\"available_providers\":[\"runware\"]}}}"}`))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/models/providers/probe", strings.NewReader(`{"model":"cline-pass/demo","upstream_model":"deepseek-v4.1-flash"}`))
	req.Header.Set("Authorization", "Bearer management-token")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status = %d, calls = %d, body = %s", recorder.Code, calls, recorder.Body.String())
	}
	var response struct {
		Pipeline  string `json:"pipeline"`
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Pipeline != "direct" || len(response.Providers) != 1 || response.Providers[0].Name != "runware" {
		t.Fatalf("probe response = %#v", response)
	}
}

func TestProbeModelProvidersDoesNotGuessPipelineWhenOrdinaryResponseHasNoProvider(t *testing.T) {
	calls := 0
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"}}]}`))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	req := httptest.NewRequest(http.MethodPost, "/api/models/providers/probe", strings.NewReader(`{"model":"cline-pass/demo","upstream_model":"deepseek-v4.1-flash"}`))
	req.Header.Set("Authorization", "Bearer management-token")
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK || calls != 1 {
		t.Fatalf("status = %d, calls = %d, body = %s", recorder.Code, calls, recorder.Body.String())
	}
	var response struct {
		OK        bool   `json:"ok"`
		Pipeline  string `json:"pipeline"`
		Error     string `json:"error"`
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || response.Pipeline != "" || len(response.Providers) != 0 || !strings.Contains(response.Error, "pipeline") {
		t.Fatalf("probe response = %#v", response)
	}
}

// Keep this test file independent of the package's JSON helper names so it
// remains obvious which routing fields are being checked.
func modelsObject(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func stringList(value any) []string {
	items, _ := value.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}
