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

	"github.com/nonlog/ClinePassProxy/internal/credentials"
)

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

func TestSSEDecoderRejectsFinalFrameWithoutBlankLine(t *testing.T) {
	// A newline terminates the data line, but only a blank line terminates the
	// SSE event. EOF here must therefore be treated as truncation.
	decoder := NewSSEDecoder(strings.NewReader("data: tail\n"), 1<<20)
	if _, err := decoder.Next(); !errors.Is(err, ErrPartialFrame) {
		t.Fatalf("expected ErrPartialFrame, got %v", err)
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
