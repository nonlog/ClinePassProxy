package translate

import (
	"encoding/json"
	"fmt"
)

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
