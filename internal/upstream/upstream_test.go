package upstream

import (
	"errors"
	"io"
	"strings"
	"testing"
)

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

func TestSSEDecoderFlushesFinalFrameWithoutBlankLine(t *testing.T) {
	decoder := NewSSEDecoder(strings.NewReader("data: tail"), 1<<20)
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
