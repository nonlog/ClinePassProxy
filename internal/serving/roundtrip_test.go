package serving

import (
	"bufio"
	"bytes"
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
	if _, err := creds.Upsert(credentials.Record{ID: "test-cred", Label: "test", APIKey: "cline-key", Enabled: true}, false); err != nil {
		t.Fatal(err)
	}
	history := admin.OpenHistory(dir, 200)
	server := New(store, creds, history)
	server.SetReady(true, "")
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
		if _, err := creds.Upsert(credentials.Record{ID: id, Label: id, APIKey: "key-" + id, Enabled: true}, false); err != nil {
			t.Fatal(err)
		}
	}
	server := New(store, creds, admin.OpenHistory(dir, 200))
	server.SetReady(true, "")
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
		if _, err := creds.Upsert(credentials.Record{ID: id, Label: id, APIKey: "key-" + id, Enabled: true}, false); err != nil {
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
	for _, stage := range []string{admin.StageRequestReceived, admin.StageTranslateDone, admin.StageUpstreamRequestStart, admin.StageFirstUpstreamEvent, admin.StageFirstDownstreamWrite, admin.StageStreamComplete} {
		if _, present := record.Timings[stage]; !present {
			t.Fatalf("timing stage %s missing: %v", stage, record.Timings)
		}
	}
	if record.PromptTokens != 5 || record.CompletionToken != 1 {
		t.Fatalf("usage not recorded: %+v", record)
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
