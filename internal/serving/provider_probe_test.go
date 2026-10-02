package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func TestProbeModelProvidersSendsBothRoutingShapes(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode probe: %v", err)
			return
		}
		gateway := modelsObject(modelsObject(body["providerOptions"])["gateway"])
		if got := stringList(gateway["only"]); len(got) != 1 || got[0] != "__probe__" {
			t.Errorf("gateway.only = %#v", got)
		}
		provider := modelsObject(body["provider"])
		if got := stringList(provider["only"]); len(got) != 1 || got[0] != "__probe__" {
			t.Errorf("provider.only = %#v", got)
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
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || len(response.Providers) != 2 || response.Providers[0].Name != "alibaba" {
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
