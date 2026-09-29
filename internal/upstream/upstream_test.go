package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

func TestJSONJoinsBaseURLPath(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer server.Close()

	for _, base := range []string{server.URL + "/api/v1", server.URL + "/api/v1/"} {
		client := New(base)
		if _, _, err := client.JSON(context.Background(), credentials.Record{APIKey: "k"}, "/users/me/plan/usage-limits", time.Second); err != nil {
			t.Fatalf("base %q: %v", base, err)
		}
		if gotPath != "/api/v1/users/me/plan/usage-limits" {
			t.Fatalf("base %q produced path %q", base, gotPath)
		}
	}
}

func TestClientBaseURLIsSafeForConcurrentUse(t *testing.T) {
	// dispatch() repoints the shared client on every request while other
	// requests are reading the same field; run it under -race.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()

	client := New(server.URL)
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < 20; iteration++ {
				client.SetBaseURL(server.URL)
				stream, err := client.ChatCompletions(context.Background(), credentials.Record{APIKey: "k"}, ChatRequest{Body: []byte(`{}`)})
				if err != nil {
					continue
				}
				stream.Close()
				_ = client.BaseURL()
			}
		}()
	}
	wait.Wait()
}

func TestSSEDecoderHandlesArbitraryBoundaries(t *testing.T) {
	stream := "event: message\ndata: {\"a\":1}\n\ndata: [DONE]\n\n"
	var events []string
	decoder := NewSSEDecoder(strings.NewReader(stream), 1<<20)
	for {
		event, err := decoder.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event.Name+"|"+string(event.Data))
	}
	if len(events) != 2 || events[0] != `message|{"a":1}` || events[1] != "|[DONE]" {
		t.Fatalf("unexpected events: %v", events)
	}
}

func TestSSEDecoderMultilineDataAndCRLF(t *testing.T) {
	stream := "data: first\r\ndata: second\r\n\r\n"
	decoder := NewSSEDecoder(strings.NewReader(stream), 1<<20)
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Data) != "first\nsecond" {
		t.Fatalf("multiline data joined as %q", event.Data)
	}
}

func TestSSEDecoderRejectsPartialFinalFrame(t *testing.T) {
	// A final line that ran into EOF was cut mid-frame; delivering it would let
	// a truncated stream look complete.
	decoder := NewSSEDecoder(strings.NewReader("data: tail"), 1<<20)
	if _, err := decoder.Next(); !errors.Is(err, ErrPartialFrame) {
		t.Fatalf("expected ErrPartialFrame, got %v", err)
	}
}

func TestSSEDecoderFlushesTerminatedFinalFrameWithoutBlankLine(t *testing.T) {
	// The line itself arrived intact, only the closing blank line is missing.
	decoder := NewSSEDecoder(strings.NewReader("data: tail\n"), 1<<20)
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Data) != "tail" {
		t.Fatalf("final frame = %q", event.Data)
	}
	if _, err := decoder.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF after the final frame, got %v", err)
	}
}

func TestSSEDecoderEnforcesLimit(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader("data: "+strings.Repeat("x", 100)+"\n\n"), 16)
	if _, err := decoder.Next(); !errors.Is(err, ErrStreamLimit) {
		t.Fatalf("expected ErrStreamLimit, got %v", err)
	}
}

func TestSSEDecoderIgnoresCommentsAndUnknownFields(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader(": keep-alive\nid: 7\nretry: 100\ndata: payload\n\n"), 1<<20)
	event, err := decoder.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Data) != "payload" {
		t.Fatalf("data = %q", event.Data)
	}
}
