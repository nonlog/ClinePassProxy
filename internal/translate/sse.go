package translate

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUpstreamTruncated reports an upstream stream that ended before the
// response finished. Silently synthesising a normal terminal frame would let a
// cut-off answer reach the client as a complete one, so the converters refuse
// to do it and the caller reports the failure instead.
var ErrUpstreamTruncated = errors.New("upstream stream ended before the response completed")

// SSEEvent renders one Server-Sent Event frame.
func SSEEvent(event string, data map[string]any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if event == "" {
		return []byte(fmt.Sprintf("data: %s\n\n", body)), nil
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, body)), nil
}

// AppendSSE renders one frame and appends it to the batch.
func AppendSSE(out *[][]byte, event string, data map[string]any) error {
	frame, err := SSEEvent(event, data)
	if err != nil {
		return err
	}
	*out = append(*out, frame)
	return nil
}

// ChatSSEEvent renders an OpenAI Chat Completions chunk frame.
func ChatSSEEvent(data map[string]any) ([]byte, error) {
	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return []byte(fmt.Sprintf("data: %s\n\n", body)), nil
}

// DoneEvent is the OpenAI-compatible terminal frame.
func DoneEvent() []byte { return []byte("data: [DONE]\n\n") }

// ChatStreamError renders the error frame that replaces the terminal [DONE]
// when a Chat Completions stream cannot be completed. The client must not be
// able to mistake a truncated answer for a finished one.
func ChatStreamError(message string) []byte {
	body, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "clinepassproxy_error"},
	})
	if err != nil {
		return []byte("data: {\"error\":{\"message\":\"upstream stream truncated\"}}\n\n")
	}
	return []byte(fmt.Sprintf("data: %s\n\n", body))
}
