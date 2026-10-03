package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestListModelsScopesObservedProvidersPerModel(t *testing.T) {
	server, handler := newTestServer(t, "http://127.0.0.1:1")
	settings := server.Config.Get()
	settings.Models = []models.Entry{
		{ID: "cline-pass/glm-5.3-flash", UpstreamID: "glm-5.3-flash"},
		{ID: "cline-pass/deepseek-v4.1-flash", UpstreamID: "deepseek-v4.1-flash"},
		{ID: "cline-pass/unseen", UpstreamID: "unseen"},
	}
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	server.History.Append(admin.Record{
		ID: "glm", Model: "cline-pass/glm-5.3-flash", Provider: "zhipu", Status: http.StatusOK, StartedAt: now,
	})
	server.History.Append(admin.Record{
		ID: "deepseek", Model: "cline-pass/deepseek-v4.1-flash", Provider: "deepseek", Status: http.StatusOK, StartedAt: now,
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	request.Header.Set("Authorization", "Bearer management-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Models []struct {
			ID                string   `json:"id"`
			ObservedProviders []string `json:"observed_providers"`
		} `json:"models"`
		ObservedProviders []string `json:"observed_providers"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Models) != 3 {
		t.Fatalf("models = %#v", response.Models)
	}
	if got := response.Models[0].ObservedProviders; len(got) != 1 || got[0] != "zhipu" {
		t.Fatalf("glm observed providers = %#v", got)
	}
	if got := response.Models[1].ObservedProviders; len(got) != 1 || got[0] != "deepseek" {
		t.Fatalf("deepseek observed providers = %#v", got)
	}
	if got := response.Models[2].ObservedProviders; len(got) != 0 {
		t.Fatalf("unseen model observed providers = %#v", got)
	}
	// Keep the top-level aggregate for existing request-filter consumers, but
	// never use it as the per-model catalog.
	if len(response.ObservedProviders) != 2 || response.ObservedProviders[0] != "deepseek" || response.ObservedProviders[1] != "zhipu" {
		t.Fatalf("aggregate observed providers = %#v", response.ObservedProviders)
	}
}
