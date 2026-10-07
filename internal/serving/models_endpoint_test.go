package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

func TestOpenAIModelsListsEnabledConfiguredAliases(t *testing.T) {
	server, handler := newTestServer(t, "http://unused.invalid")
	settings := server.Config.Get()
	settings.Models = []models.Entry{
		{ID: "deepseek-v4.1-flash", UpstreamID: "cline-pass/deepseek-v4.1-flash"},
		{ID: "glm-5.3-flash", UpstreamID: "cline-pass/glm-5.3-flash"},
		{ID: "disabled-model", UpstreamID: "cline-pass/disabled", Disabled: true},
	}
	if _, err := server.Config.Update(settings); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string `json:"id"`
			Object  string `json:"object"`
			Created int    `json:"created"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Object != "list" {
		t.Fatalf("object = %q", payload.Object)
	}
	if len(payload.Data) != 2 {
		t.Fatalf("models = %#v", payload.Data)
	}
	if payload.Data[0].ID != "deepseek-v4.1-flash" || payload.Data[1].ID != "glm-5.3-flash" {
		t.Fatalf("unexpected model ids: %#v", payload.Data)
	}
	for _, model := range payload.Data {
		if model.Object != "model" || model.Created != 0 || model.OwnedBy != "ClinePassProxy" {
			t.Fatalf("unexpected model object: %#v", model)
		}
	}
}

func TestOpenAIModelsRequiresInferenceKey(t *testing.T) {
	_, handler := newTestServer(t, "http://unused.invalid")
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}
