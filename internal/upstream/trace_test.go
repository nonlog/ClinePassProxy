package upstream

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

func TestChatCompletionsTraceConnectionReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()
	client := New(server.URL)

	for attempt := 0; attempt < 2; attempt++ {
		trace := NewTrace(time.Now())
		stream, err := client.ChatCompletions(context.Background(), credentials.Record{APIKey: "private-test-key"}, ChatRequest{
			Body: []byte(`{}`), Trace: trace,
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(io.Discard, stream.Body)
		stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		snapshot := trace.Snapshot()
		if snapshot.Protocol != "HTTP/1.1" || snapshot.StatusCode != http.StatusOK {
			t.Fatalf("missing response metadata: %+v", snapshot)
		}
		if snapshot.ConnReused != (attempt == 1) || (attempt == 1 && !snapshot.WasIdle) {
			t.Fatalf("attempt %d connection metadata: %+v", attempt, snapshot)
		}
		for _, name := range []string{"get_conn", "got_conn", "wrote_headers", "wrote_request", "first_response_byte", "response_headers"} {
			traceEvent(t, snapshot, name)
		}
		if attempt == 0 {
			traceEvent(t, snapshot, "connect_start")
			traceEvent(t, snapshot, "connect_done")
		}
		if event := traceEvent(t, snapshot, "wrote_request"); event.Success == nil || !*event.Success {
			t.Fatalf("request write was not recorded as successful: %+v", event)
		}
	}
}

func TestChatCompletionsTraceSeparatesUploadAndHeaderWait(t *testing.T) {
	const headerDelay = 60 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(headerDelay)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	// All events share an earlier request start, not the time the HTTP call began.
	trace := NewTrace(time.Now().Add(-time.Second))
	stream, err := New(server.URL).ChatCompletions(context.Background(), credentials.Record{APIKey: "k"}, ChatRequest{
		Body: []byte(strings.Repeat("x", 1<<20)), Trace: trace,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	snapshot := trace.Snapshot()
	wrote := traceEvent(t, snapshot, "wrote_request")
	firstByte := traceEvent(t, snapshot, "first_response_byte")
	headers := traceEvent(t, snapshot, "response_headers")
	if wrote.AtMS < 1000 || firstByte.AtMS-wrote.AtMS < 40 || headers.AtMS < firstByte.AtMS {
		t.Fatalf("upload/header timings do not expose the delayed response: %+v", snapshot.Events)
	}
}

func TestTraceConcurrentSnapshotsAreBoundedAndPrivate(t *testing.T) {
	trace := NewTrace(time.Now())
	hooks := trace.ClientTrace()
	private := "private-host-and-error"
	hooks.GetConn(private)
	hooks.DNSStart(httptrace.DNSStartInfo{Host: private})
	hooks.DNSDone(httptrace.DNSDoneInfo{Addrs: []net.IPAddr{{IP: net.ParseIP("192.0.2.1")}}, Err: errors.New(private)})
	hooks.ConnectStart(private, private)
	hooks.ConnectDone(private, private, errors.New(private))
	hooks.TLSHandshakeStart()
	hooks.TLSHandshakeDone(tls.ConnectionState{ServerName: private}, errors.New(private))
	hooks.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New(private)})

	copy := trace.Snapshot()
	copy.Events[0].Name = private
	*copy.Events[2].Success = true
	if snapshot := trace.Snapshot(); snapshot.Events[0].Name != "get_conn" || *snapshot.Events[2].Success {
		t.Fatal("snapshot shares mutable event state")
	}

	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 100; i++ {
				hooks.GotConn(httptrace.GotConnInfo{Reused: true, WasIdle: true, IdleTime: time.Second})
				hooks.WroteHeaders()
				hooks.GotFirstResponseByte()
				trace.Response("HTTP/2.0", http.StatusOK)
				_ = trace.Snapshot()
			}
		}()
	}
	wait.Wait()
	snapshot := trace.Snapshot()
	if len(snapshot.Events) != maxTraceEvents || !snapshot.ConnReused || snapshot.IdleTimeMS != 1000 {
		t.Fatalf("trace was not bounded or lost metadata: %+v", snapshot)
	}
	for i := 1; i < len(snapshot.Events); i++ {
		if snapshot.Events[i].AtMS < snapshot.Events[i-1].AtMS {
			t.Fatal("trace events are not in timestamp order")
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), private) || strings.Contains(string(encoded), "192.0.2.1") {
		t.Fatalf("trace exposes private callback data: %s", encoded)
	}
}

func traceEvent(t *testing.T, snapshot TraceSnapshot, name string) TraceEvent {
	t.Helper()
	for _, event := range snapshot.Events {
		if event.Name == name {
			return event
		}
	}
	t.Fatalf("missing event %q in %+v", name, snapshot.Events)
	return TraceEvent{}
}
