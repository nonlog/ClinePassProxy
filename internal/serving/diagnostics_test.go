package serving

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
)

type delayedFlush struct{ *httptest.ResponseRecorder }

func (w delayedFlush) Flush() { time.Sleep(20 * time.Millisecond); w.ResponseRecorder.Flush() }

func TestOutputTimingIsAfterFlushAndSeparatesReasoningTextTool(t *testing.T) {
	tracker := admin.NewTracker()
	writer := newSSEWriter(delayedFlush{httptest.NewRecorder()})
	reason := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hidden\"}}\n\n")
	text := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"visible\"}}\n\n")
	tool := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\"}}\n\n")
	if err := writer.write(tracker, [][]byte{reason}); err != nil {
		t.Fatal(err)
	}
	before := tracker.Finish(http.StatusOK, nil)
	if before.VisibleTextTTFTMS != nil || before.TTFTMS != 0 {
		t.Fatalf("reasoning reported as visible text: %+v", before)
	}
	if before.ReasoningTTFTMS == nil || *before.ReasoningTTFTMS < 20 {
		t.Fatalf("reasoning timing precedes flush: %+v", before)
	}
	for _, frame := range [][]byte{text, tool} {
		if err := writer.write(tracker, [][]byte{frame}); err != nil {
			t.Fatal(err)
		}
	}
	after := tracker.Finish(http.StatusOK, nil)
	if after.VisibleTextTTFTMS == nil || after.ToolTTFTMS == nil || *after.VisibleTextTTFTMS <= *after.ReasoningTTFTMS || *after.ToolTTFTMS <= *after.VisibleTextTTFTMS {
		t.Fatalf("output kinds collapsed: %+v", after)
	}
	if after.ResponseBytes != int64(len(reason)+len(text)+len(tool)) {
		t.Fatalf("response bytes=%d", after.ResponseBytes)
	}
}

func TestToolOnlyResponsesTurnHasNoVisibleTextTTFT(t *testing.T) {
	cline := stubCline(t, []string{
		`{"choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"completion_tokens_details":{"reasoning_tokens":2}}}`,
	})
	defer cline.Close()
	server, handler := newTestServer(t, cline.URL)
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object"}}]}`))
	r.Header.Set("Authorization", "Bearer gateway-key")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	records := server.History.List(1, "")
	if len(records) != 1 {
		t.Fatalf("records=%d", len(records))
	}
	record := records[0]
	if record.Status != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("stream failed: %s", w.Body.String())
	}
	if record.VisibleTextTTFTMS != nil || record.TTFTMS != 0 || record.ReasoningTTFTMS == nil || record.ToolTTFTMS == nil {
		t.Fatalf("tool-only turn timing: %+v", record)
	}
	if _, ok := record.Timings[admin.StageFirstReasoningEvent]; !ok {
		t.Fatal("missing reasoning arrival")
	}
	if len(record.Transport) != 1 || record.Transport[0].StatusCode != 200 {
		t.Fatalf("missing transport attempt: %+v", record.Transport)
	}
}
