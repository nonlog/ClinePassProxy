package serving

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

const wrappedPlannerCompletion = `{"success":true,"data":{"choices":[{"message":{"content":"ready","provider_metadata":{"gateway":{"routing":{"finalProvider":"runware","canonicalSlug":"deepseek/model"}}}}}]}}`

func TestModelTestWrappedProviderResponse(t *testing.T) {
	tests := []struct {
		name               string
		providers          []string
		pipeline           string
		body               string
		wantOK             bool
		wantProvider       string
		wantActualProvider string
		wantReply          string
		wantError          string
	}{
		{
			name:         "automatic routing returns actual provider and reply",
			body:         wrappedPlannerCompletion,
			wantOK:       true,
			wantProvider: "runware",
			wantReply:    "ready",
		},
		{
			name:               "planner pin rejects wrapped provider mismatch",
			providers:          []string{"runware"},
			pipeline:           "planner",
			body:               strings.Replace(wrappedPlannerCompletion, `"finalProvider":"runware"`, `"finalProvider":"other"`, 1),
			wantActualProvider: "other",
			wantError:          "not honored",
		},
		{
			name:      "planner pin requires actual provider evidence",
			providers: []string{"runware"},
			pipeline:  "planner",
			body:      `{"success":true,"data":{"choices":[{"message":{"content":"ready"}}]}}`,
			wantError: "unable to verify",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var upstreamBody map[string]any
			cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
					t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				if err := json.NewDecoder(r.Body).Decode(&upstreamBody); err != nil {
					t.Errorf("decode upstream request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer cline.Close()

			server, handler := newTestServer(t, cline.URL)
			settings := server.Config.Get()
			settings.Models = []models.Entry{{
				ID:               "m",
				UpstreamID:       "deepseek/model",
				Providers:        test.providers,
				ProviderPipeline: test.pipeline,
			}}
			if _, err := server.Config.Update(settings); err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, "/api/models/test", strings.NewReader(`{"model":"m"}`))
			request.Header.Set("Authorization", "Bearer "+settings.ManagementToken)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if len(test.providers) == 0 {
				if upstreamBody["provider"] != nil || upstreamBody["providerOptions"] != nil {
					t.Fatalf("automatic routing sent provider selections: %#v", upstreamBody)
				}
			} else {
				gateway := models.Object(models.Object(upstreamBody["providerOptions"])["gateway"])
				if only := models.List(gateway["only"]); len(only) != 1 || models.String(only[0]) != "runware" {
					t.Fatalf("providerOptions.gateway.only = %#v", gateway["only"])
				}
				if upstreamBody["provider"] != nil {
					t.Fatalf("planner pin sent direct provider shape: %#v", upstreamBody["provider"])
				}
			}

			var response struct {
				OK             bool   `json:"ok"`
				Provider       string `json:"provider"`
				ActualProvider string `json:"actual_provider"`
				Reply          string `json:"reply"`
				Error          string `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.OK != test.wantOK {
				t.Errorf("ok = %t, want %t; response = %#v", response.OK, test.wantOK, response)
			}
			if !test.wantOK && response.ActualProvider != test.wantActualProvider {
				t.Errorf("actual_provider = %q, want %q", response.ActualProvider, test.wantActualProvider)
			}
			if test.wantOK {
				if response.Provider != test.wantProvider {
					t.Errorf("provider = %q, want %q", response.Provider, test.wantProvider)
				}
				if response.Reply != test.wantReply {
					t.Errorf("reply = %q, want %q", response.Reply, test.wantReply)
				}
			}
			if test.wantError == "" {
				if response.Error != "" {
					t.Errorf("unexpected error = %q", response.Error)
				}
			} else if !strings.Contains(strings.ToLower(response.Error), test.wantError) || !strings.Contains(strings.ToLower(response.Error), "provider") {
				t.Errorf("error = %q, want explicit %q error", response.Error, test.wantError)
			}
		})
	}
}

func TestWrappedProviderResponseExtraction(t *testing.T) {
	t.Run("pipeline", func(t *testing.T) {
		pipeline, provider := providerPipelineFromResponse([]byte(wrappedPlannerCompletion))
		if pipeline != "planner" || provider != "runware" {
			t.Fatalf("pipeline/provider = %q/%q, want planner/runware", pipeline, provider)
		}
	})
	t.Run("actual provider", func(t *testing.T) {
		if provider := probeActualProvider([]byte(wrappedPlannerCompletion)); provider != "runware" {
			t.Fatalf("actual provider = %q, want runware", provider)
		}
	})
}

func TestProbeModelProvidersRoutingContract(t *testing.T) {
	privateCompletion := strings.Replace(wrappedPlannerCompletion, `"finalProvider":"runware"`, `"finalProvider":"private"`, 1)
	plannerStream := "data: " + `{"choices":[{"index":0,"delta":{"content":"ready","provider_metadata":{"gateway":{"routing":{"finalProvider":"runware","canonicalSlug":"deepseek/model"}}}}}]}` + "\n\n" +
		"data: " + `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
	type probeStep struct {
		stream    bool
		maxTokens int64
		pipeline  string
		status    int
		body      string
	}
	tests := []struct {
		name               string
		steps              []probeStep
		wantOK             bool
		wantPinningStatus  string
		wantPipeline       string
		wantActualProvider string
		wantProviders      []string
		wantError          string
	}{
		{
			name: "planner completion ignoring invalid provider is not pinnable",
			steps: []probeStep{
				{maxTokens: 64, status: http.StatusOK, body: privateCompletion},
				{maxTokens: 16, pipeline: "planner", status: http.StatusOK, body: privateCompletion},
			},
			wantPinningStatus:  "routing_ignored",
			wantPipeline:       "planner",
			wantActualProvider: "private",
			wantError:          "ignored",
		},
		{
			name: "empty content retries identification as SSE with same model and budget",
			steps: []probeStep{
				{maxTokens: 64, status: http.StatusInternalServerError, body: `{"error":"empty response content"}`},
				{stream: true, maxTokens: 64, status: http.StatusOK, body: plannerStream},
				{maxTokens: 16, pipeline: "planner", status: http.StatusBadRequest, body: `{"error":{"message":"Available providers are: runware, alibaba."}}`},
			},
			wantOK:             true,
			wantPinningStatus:  "catalog_available",
			wantPipeline:       "planner",
			wantActualProvider: "runware",
			wantProviders:      []string{"runware", "alibaba"},
		},
		{
			name: "wrapped direct response uses only direct catalog probe",
			steps: []probeStep{
				{maxTokens: 64, status: http.StatusOK, body: `{"success":true,"data":{"provider":"GMICloud","model":"deepseek/model","choices":[{"message":{"content":"ready"}}]}}`},
				{maxTokens: 16, pipeline: "direct", status: http.StatusBadRequest, body: `{"error":{"metadata":{"available_providers":["runware"]}}}`},
			},
			wantOK:             true,
			wantPinningStatus:  "catalog_available",
			wantPipeline:       "direct",
			wantActualProvider: "GMICloud",
			wantProviders:      []string{"runware"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/chat/completions" {
					t.Errorf("upstream request = %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				calls++
				if calls > len(test.steps) {
					t.Errorf("unexpected upstream call %d", calls)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				step := test.steps[calls-1]
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream call %d: %v", calls, err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if models.String(body["model"]) != "cline-pass/demo" {
					t.Errorf("call %d model = %#v, want original cline-pass/demo", calls, body["model"])
				}
				if models.Number(body["max_tokens"]) != step.maxTokens {
					t.Errorf("call %d max_tokens = %#v, want %d", calls, body["max_tokens"], step.maxTokens)
				}
				if models.Bool(body["stream"]) != step.stream {
					t.Errorf("call %d stream = %#v, want %t", calls, body["stream"], step.stream)
				}
				switch step.pipeline {
				case "":
					if body["provider"] != nil || body["providerOptions"] != nil {
						t.Errorf("identification call %d sent provider selections: %#v", calls, body)
					}
				case "planner":
					gateway := models.Object(models.Object(body["providerOptions"])["gateway"])
					if only := models.List(gateway["only"]); len(only) != 1 || models.String(only[0]) != "__probe__" {
						t.Errorf("planner call %d gateway.only = %#v", calls, gateway["only"])
					}
					if body["provider"] != nil {
						t.Errorf("planner call %d sent direct provider shape: %#v", calls, body["provider"])
					}
				case "direct":
					provider := models.Object(body["provider"])
					if only := models.List(provider["only"]); len(only) != 1 || models.String(only[0]) != "__probe__" {
						t.Errorf("direct call %d provider.only = %#v", calls, provider["only"])
					}
					if body["providerOptions"] != nil {
						t.Errorf("direct call %d sent planner provider shape: %#v", calls, body["providerOptions"])
					}
				}
				if step.stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(step.status)
				_, _ = w.Write([]byte(step.body))
			}))
			defer cline.Close()

			server, handler := newTestServer(t, cline.URL)
			request := httptest.NewRequest(http.MethodPost, "/api/models/providers/probe", strings.NewReader(`{"model":"demo","upstream_model":"cline-pass/demo"}`))
			request.Header.Set("Authorization", "Bearer "+server.Config.Get().ManagementToken)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			if calls != len(test.steps) {
				t.Errorf("upstream calls = %d, want %d", calls, len(test.steps))
			}
			var response struct {
				OK             bool   `json:"ok"`
				Pinnable       bool   `json:"pinnable"`
				PinningStatus  string `json:"pinning_status"`
				Pipeline       string `json:"pipeline"`
				ActualProvider string `json:"actual_provider"`
				CanonicalSlug  string `json:"canonical_slug"`
				Status         int    `json:"status"`
				Error          string `json:"error"`
				Providers      []struct {
					Name string `json:"name"`
				} `json:"providers"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.OK != test.wantOK || response.Pinnable != test.wantOK {
				t.Errorf("ok/pinnable = %t/%t, want %t/%t; response = %#v", response.OK, response.Pinnable, test.wantOK, test.wantOK, response)
			}
			if response.PinningStatus != test.wantPinningStatus {
				t.Errorf("pinning_status = %q, want %q", response.PinningStatus, test.wantPinningStatus)
			}
			if response.Pipeline != test.wantPipeline {
				t.Errorf("pipeline = %q, want %q", response.Pipeline, test.wantPipeline)
			}
			if response.ActualProvider != test.wantActualProvider {
				t.Errorf("actual_provider = %q, want %q", response.ActualProvider, test.wantActualProvider)
			}
			if response.CanonicalSlug != "deepseek/model" {
				t.Errorf("canonical_slug = %q, want deepseek/model", response.CanonicalSlug)
			}
			if wantStatus := test.steps[len(test.steps)-1].status; response.Status != wantStatus {
				t.Errorf("status = %d, want %d", response.Status, wantStatus)
			}
			providers := make([]string, 0, len(response.Providers))
			for _, provider := range response.Providers {
				providers = append(providers, provider.Name)
			}
			if strings.Join(providers, ",") != strings.Join(test.wantProviders, ",") {
				t.Errorf("providers = %#v, want %#v", providers, test.wantProviders)
			}
			if test.wantError == "" {
				if response.Error != "" {
					t.Errorf("unexpected error = %q", response.Error)
				}
			} else if !strings.Contains(strings.ToLower(response.Error), test.wantError) {
				t.Errorf("error = %q, want %q", response.Error, test.wantError)
			}
		})
	}
}
