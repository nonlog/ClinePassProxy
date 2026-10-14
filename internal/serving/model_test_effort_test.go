package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// The per-model reasoning selector was introduced upstream as a management
// probe option. It must never change ordinary inference or silently map a
// client's reasoning effort.
func TestModelTestReasoningEffortPassThrough(t *testing.T) {
	tests := []struct {
		name         string
		effort       string
		expectStatus int
		expectEffort string
		wantUpstream bool
	}{
		{"unset", "", http.StatusOK, "", true},
		{"minimal", "minimal", http.StatusOK, "minimal", true},
		{"max", "max", http.StatusOK, "max", true},
		{"invalid", "ultra", http.StatusBadRequest, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests []map[string]any
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode upstream: %v", err)
					return
				}
				requests = append(requests, request)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"chatcmpl-test","model":"demo","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ready"}}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`))
			}))
			defer upstream.Close()
			server, handler := newTestServer(t, upstream.URL)
			settings := server.Config.Get()
			settings.Models = []models.Entry{{ID: "demo", UpstreamID: "demo"}}
			if _, err := server.Config.Update(settings); err != nil {
				t.Fatal(err)
			}

			body, err := json.Marshal(map[string]any{"model": "demo", "credential_id": "test-cred", "reasoning_effort": tc.effort})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/models/test", strings.NewReader(string(body)))
			req.Header.Set("Authorization", "Bearer management-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.expectStatus {
				t.Fatalf("status=%d want=%d response=%s", rec.Code, tc.expectStatus, rec.Body.String())
			}
			if !tc.wantUpstream {
				if len(requests) != 0 {
					t.Fatalf("invalid effort triggered upstream: %v", requests)
				}
				return
			}
			if len(requests) != 1 {
				t.Fatalf("expected 1 upstream probe, got %d body=%s", len(requests), rec.Body.String())
			}
			if got := models.String(requests[0]["reasoning_effort"]); got != tc.expectEffort {
				t.Fatalf("upstream reasoning_effort=%q, want %q", got, tc.expectEffort)
			}
			if _, present := requests[0]["reasoning_effort"]; present && tc.expectEffort == "" {
				t.Fatal("empty effort should be omitted from request")
			}
		})
	}
}
