package serving

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/models"
)

// stubCline emits a deterministic Chat Completions stream, writing each chunk
// through a tiny writer so the proxy must handle arbitrary read boundaries.
func stubCline(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		stream := r.URL.Query().Get("stream") == "true"
		_ = stream
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range chunks {
			frame := "data: " + chunk + "\n\n"
			// Split into 5-byte writes to force partial SSE frames downstream.
			for i := 0; i < len(frame); i += 5 {
				end := i + 5
				if end > len(frame) {
					end = len(frame)
				}
				_, _ = w.Write([]byte(frame[i:end]))
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
}

func newTestServer(t *testing.T, baseURL string) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = baseURL
	settings.GatewayKeys = []string{"gateway-key"}
	settings.ManagementToken = "management-token"
	settings.Affinity = config.AffinitySession
	settings.TimeoutSeconds = 30
	settings.RetryFailover = 1
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Upsert(credentials.Record{ID: "test-cred", Label: "test", APIKey: "cline-key", Enabled: true}, false, false); err != nil {
		t.Fatal(err)
	}
	history := admin.OpenHistory(dir, 200)
	server := New(store, creds, history)
	return server, server.Handler()
}

// newTestServerWithCredentials builds a proxy with a fixed credential pool.
func newTestServerWithCredentials(t *testing.T, baseURL string, ids ...string) (*Server, http.Handler) {
	t.Helper()
	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = baseURL
	settings.GatewayKeys = []string{"gateway-key"}
	settings.ManagementToken = "management-token"
	settings.Affinity = config.AffinitySession
	settings.TimeoutSeconds = 30
	settings.RetryFailover = 1
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := creds.Upsert(credentials.Record{ID: id, Label: id, APIKey: "key-" + id, Enabled: true}, false, false); err != nil {
			t.Fatal(err)
		}
	}
	server := New(store, creds, admin.OpenHistory(dir, 200))
	return server, server.Handler()
}

func TestMessagesStreamingRoundTrip(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"lo"}}]}`,
		`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":8}}}`,
	}
	cline := stubCline(t, chunks)
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"claude-sonnet-4.6","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}
	events := readEvents(t, recorder.Body.Bytes())
	for _, want := range []string{"message_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(events, want) {
			t.Fatalf("missing %s in stream:\n%s", want, events)
		}
	}
	if !strings.Contains(events, `"text":"Hel"`) || !strings.Contains(events, `"text":"lo"`) {
		t.Fatalf("output was coalesced or truncated:\n%s", events)
	}
	if !strings.Contains(events, `"cache_read_input_tokens":8`) {
		t.Fatalf("cache tokens missing:\n%s", events)
	}
}

func TestMessagesNonStreamingUnwrapsClineEnvelope(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chatcmpl-env","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"unwrapped"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":4}}}}`))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var message map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &message); err != nil {
		t.Fatalf("non-streaming reply is not JSON: %v (%s)", err, recorder.Body.String())
	}
	content := models.List(message["content"])
	if len(content) == 0 || models.String(models.Object(content[0])["text"]) != "unwrapped" {
		t.Fatalf("envelope was not unwrapped: %s", recorder.Body.String())
	}
	usage := models.Object(message["usage"])
	if models.Number(usage["cache_read_input_tokens"]) != 4 {
		t.Fatalf("cache tokens lost through the envelope: %v", usage)
	}
}

func TestMessagesStreamingUnwrapsClineEnvelope(t *testing.T) {
	chunks := []string{
		`{"success":true,"data":{"id":"chatcmpl-env","choices":[{"index":0,"delta":{"content":"wrapped"}}]}}`,
		`{"success":true,"data":{"id":"chatcmpl-env","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}}`,
	}
	cline := stubCline(t, chunks)
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	events := readEvents(t, recorder.Body.Bytes())
	if !strings.Contains(events, `"text":"wrapped"`) {
		t.Fatalf("streamed envelope was not unwrapped:\n%s", events)
	}
}

func TestResponsesStreamingRoundTrip(t *testing.T) {
	chunks := []string{
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"part"}}]}`,
		`{"id":"chatcmpl-2","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":1}}`,
	}
	cline := stubCline(t, chunks)
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"gpt-5.6-terra","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	events := readEvents(t, recorder.Body.Bytes())
	for _, want := range []string{"response.created", "response.in_progress", "response.output_text.delta", "response.completed"} {
		if !strings.Contains(events, want) {
			t.Fatalf("missing %s in stream:\n%s", want, events)
		}
	}
}

func TestGatewayKeyIsRequired(t *testing.T) {
	cline := stubCline(t, []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`})
	defer cline.Close()
	_, handler := newTestServer(t, cline.URL)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"x"}]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated inference must be rejected, got %d", recorder.Code)
	}
}

func TestManagementTokenIsSeparateFromGatewayKey(t *testing.T) {
	cline := stubCline(t, []string{})
	defer cline.Close()
	_, handler := newTestServer(t, cline.URL)

	request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("gateway key must not authenticate management, got %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	request.Header.Set("Authorization", "Bearer management-token")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("management token must authenticate, got %d", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recorder.Body.String(), "cline-key") {
		t.Fatalf("management status leaked a credential: %s", recorder.Body.String())
	}
}

func TestCredentialSecretsAreNeverReturned(t *testing.T) {
	cline := stubCline(t, []string{})
	defer cline.Close()
	_, handler := newTestServer(t, cline.URL)

	request := httptest.NewRequest(http.MethodGet, "/api/credentials", nil)
	request.Header.Set("Authorization", "Bearer management-token")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if strings.Contains(recorder.Body.String(), "cline-key") {
		t.Fatalf("credential listing leaked the API key: %s", recorder.Body.String())
	}
}

func TestAffinityIsStableAcrossTurns(t *testing.T) {
	var seen []string
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer cline.Close()

	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = cline.URL
	settings.GatewayKeys = []string{"gateway-key"}
	settings.TimeoutSeconds = 30
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := creds.Upsert(credentials.Record{ID: id, Label: id, APIKey: "key-" + id, Enabled: true}, false, false); err != nil {
			t.Fatal(err)
		}
	}
	handler := New(store, creds, admin.OpenHistory(dir, 100)).Handler()

	for turn := 0; turn < 6; turn++ {
		body := fmt.Sprintf(`{"model":"m","stream":true,"prompt_cache_key":"session-42","messages":[{"role":"user","content":"turn %d"}]}`, turn)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer gateway-key")
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}
	if len(seen) != 6 {
		t.Fatalf("expected 6 upstream calls, got %d", len(seen))
	}
	for _, header := range seen {
		if header != seen[0] {
			t.Fatalf("affinity drifted across turns: %v", seen)
		}
	}
}

func TestRequestHistoryRecordsStages(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"content":"x"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`,
	}
	cline := stubCline(t, chunks)
	defer cline.Close()

	server, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	handler.ServeHTTP(httptest.NewRecorder(), request)

	records := server.History.List(1, "")
	if len(records) != 1 {
		t.Fatalf("expected one request record, got %d", len(records))
	}
	record := records[0]
	for _, stage := range []string{admin.StageRequestReceived, admin.StageTranslateDone, admin.StageUpstreamRequestStart, admin.StageFirstUpstreamEvent, admin.StageFirstDownstreamWrite, admin.StageFirstTokenWrite, admin.StageStreamComplete} {
		if _, present := record.Timings[stage]; !present {
			t.Fatalf("timing stage %s missing: %v", stage, record.Timings)
		}
	}
	// TTFT must measure the first visible token, not the protocol prologue that
	// is written before the provider has produced anything.
	if record.TTFTMS <= 0 || record.TTFTMS < record.Timings[admin.StageFirstUpstreamEvent] {
		t.Fatalf("ttft = %d, first upstream event at %d", record.TTFTMS, record.Timings[admin.StageFirstUpstreamEvent])
	}
	if record.PromptTokens != 5 || record.CompletionToken != 1 {
		t.Fatalf("usage not recorded: %+v", record)
	}
}

func TestClientCancelCancelsUpstream(t *testing.T) {
	canceled := make(chan struct{})
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-time.After(15 * time.Second):
		}
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer gateway-key")
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	handler.ServeHTTP(httptest.NewRecorder(), request)

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request survived the client disconnecting")
	}
}

func TestTruncatedStreamIsReportedNotCompleted(t *testing.T) {
	// The upstream sends content and closes without a finish reason.
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer cline.Close()

	server, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	events := readEvents(t, recorder.Body.Bytes())
	if !strings.Contains(events, "event: error") {
		t.Fatalf("truncated stream was not reported as an error:\n%s", events)
	}
	if strings.Contains(events, "message_stop") {
		t.Fatalf("truncated stream was closed as a completed turn:\n%s", events)
	}
	records := server.History.List(1, "")
	if len(records) != 1 || records[0].Status != http.StatusBadGateway {
		t.Fatalf("truncated stream recorded as %+v", records)
	}
}

func TestPartialSSEFrameIsReportedNotCompleted(t *testing.T) {
	// The upstream is cut off mid-frame: no closing newline, no [DONE].
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"cut"}}]}`))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	events := readEvents(t, recorder.Body.Bytes())
	if !strings.Contains(events, "event: error") {
		t.Fatalf("partial SSE frame was not reported:\n%s", events)
	}
	if strings.Contains(events, "message_stop") {
		t.Fatalf("partial SSE frame was closed as a completed turn:\n%s", events)
	}
}

func TestResponsesTruncationIsReportedNotCompleted(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n"))
	}))
	defer cline.Close()

	_, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	events := readEvents(t, recorder.Body.Bytes())
	if !strings.Contains(events, "response.failed") {
		t.Fatalf("truncated Responses stream was not reported:\n%s", events)
	}
	if strings.Contains(events, "response.completed") {
		t.Fatalf("truncated Responses stream was closed as complete:\n%s", events)
	}
}

func TestCredentialEditKeepsDisabledStateAndClearsProxy(t *testing.T) {
	cline := stubCline(t, []string{})
	defer cline.Close()
	_, handler := newTestServer(t, cline.URL)

	call := func(method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer management-token")
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		var payload map[string]any
		_ = json.Unmarshal(recorder.Body.Bytes(), &payload)
		return recorder, payload
	}

	recorder, created := call(http.MethodPost, "/api/credentials",
		`{"label":"secondary","api_key":"secret-key","enabled":false,"proxy_url":"socks5://127.0.0.1:1080"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	id := models.String(created["id"])
	if id == "" {
		t.Fatal("created credential has no id")
	}
	if enabled, _ := created["enabled"].(bool); enabled {
		t.Fatal("credential was not created disabled")
	}

	// The edit form does not send `enabled`; the stored state must survive.
	recorder, updated := call(http.MethodPut, "/api/credentials/"+id, `{"label":"secondary","api_key":"","proxy_url":""}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if enabled, _ := updated["enabled"].(bool); enabled {
		t.Fatalf("editing a disabled credential re-enabled it: %s", recorder.Body.String())
	}
	if proxy := models.String(updated["proxy"]); proxy != "" {
		t.Fatalf("an explicit blank proxy_url did not clear the stored proxy: %q", proxy)
	}

	// Omitting proxy_url keeps the stored proxy.
	if recorder, _ = call(http.MethodPut, "/api/credentials/"+id, `{"proxy_url":"http://127.0.0.1:8080"}`); recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	recorder, updated = call(http.MethodPut, "/api/credentials/"+id, `{"label":"renamed"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("update status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if proxy := models.String(updated["proxy"]); proxy != "http://127.0.0.1:8080" {
		t.Fatalf("an omitted proxy_url dropped the stored proxy: %q", proxy)
	}
}

func TestReadinessFollowsCredentialPool(t *testing.T) {
	cline := stubCline(t, []string{})
	defer cline.Close()

	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = cline.URL
	settings.GatewayKeys = []string{"gateway-key"}
	settings.ManagementToken = "management-token"
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(store, creds, admin.OpenHistory(dir, 50)).Handler()

	ready := func() int {
		request := httptest.NewRequest(http.MethodGet, "/ready", nil)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	// Starting with an empty pool must not pin readiness to 503 forever.
	if status := ready(); status != http.StatusServiceUnavailable {
		t.Fatalf("empty pool reported ready: %d", status)
	}
	if _, err := creds.Upsert(credentials.Record{ID: "late", Label: "late", APIKey: "k", Enabled: true}, false, false); err != nil {
		t.Fatal(err)
	}
	if status := ready(); status != http.StatusOK {
		t.Fatalf("readiness did not follow the pool: %d", status)
	}
	if _, err := creds.SetEnabled("late", false); err != nil {
		t.Fatal(err)
	}
	if status := ready(); status != http.StatusServiceUnavailable {
		t.Fatalf("disabling the last credential kept the service ready: %d", status)
	}
}

func TestInferenceIsClosedWithoutGatewayKey(t *testing.T) {
	cline := stubCline(t, []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`})
	defer cline.Close()

	dir := t.TempDir()
	settings := config.Defaults()
	settings.DataDir = dir
	settings.BaseURL = cline.URL
	settings.ManagementToken = "management-token"
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(settings); err != nil {
		t.Fatal(err)
	}
	creds, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Upsert(credentials.Record{ID: "c", Label: "c", APIKey: "k", Enabled: true}, false, false); err != nil {
		t.Fatal(err)
	}
	handler := New(store, creds, admin.OpenHistory(dir, 50)).Handler()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("inference without a gateway key returned %d", recorder.Code)
	}

	// The explicit opt-out re-opens the data plane.
	current := store.Get()
	current.AllowUnauthenticated = true
	if _, err := store.Update(current); err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	handler.ServeHTTP(recorder, request)
	if recorder.Code == http.StatusServiceUnavailable || recorder.Code == http.StatusUnauthorized {
		t.Fatalf("allow_unauthenticated did not open the data plane: %d", recorder.Code)
	}
}

func TestUpstreamErrorIsReportedAndRetried(t *testing.T) {
	var attempts int
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer cline.Close()

	_, handler := newTestServerWithCredentials(t, cline.URL, "primary", "secondary")
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("failover did not recover: %d %s", recorder.Code, recorder.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("expected one retry after a 429, got %d attempts", attempts)
	}
}

func TestFailedRequestRecordsStatusAndDuration(t *testing.T) {
	cline := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"Insufficient balance. Your Cline Credits balance is $-0.08"}}`))
	}))
	defer cline.Close()

	server, handler := newTestServer(t, cline.URL)
	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer gateway-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "Insufficient balance") {
		t.Fatalf("upstream message was not surfaced: %s", recorder.Body.String())
	}
	records := server.History.List(1, "")
	if len(records) != 1 {
		t.Fatalf("expected a recorded failure, got %d records", len(records))
	}
	record := records[0]
	if record.Status != http.StatusPaymentRequired {
		t.Fatalf("recorded status = %d, want 402", record.Status)
	}
	if record.DurationMS <= 0 && len(record.Timings) == 0 {
		t.Fatalf("recorded failure has neither duration nor timings: %+v", record)
	}
	if _, complete := record.Timings[admin.StageRequestComplete]; !complete {
		t.Fatalf("failure record is missing the terminal stage: %v", record.Timings)
	}
	if record.FailoverCount != 1 || len(record.Attempts) != 1 {
		t.Fatalf("attempt trail not recorded: %+v", record.Attempts)
	}
}

// readEvents normalizes an SSE body so substring assertions survive framing.
func readEvents(t *testing.T, body []byte) string {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var builder strings.Builder
	for scanner.Scan() {
		builder.WriteString(scanner.Text())
		builder.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return builder.String()
}

func TestHistoryRetentionApplies(t *testing.T) {
	dir := t.TempDir()
	history := admin.OpenHistory(dir, 50)
	for i := 0; i < 60; i++ {
		history.Append(admin.Record{ID: fmt.Sprintf("r%d", i), StartedAt: time.Now().UTC(), Status: 200})
	}
	if got := len(history.List(0, "")); got != 50 {
		t.Fatalf("retention kept %d records, want 50", got)
	}
	reopened := admin.OpenHistory(dir, 50)
	if got := len(reopened.List(0, "")); got != 50 {
		t.Fatalf("persisted retention kept %d records, want 50", got)
	}
}
