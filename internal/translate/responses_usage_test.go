package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResponsesUsageReasoningTokens(t *testing.T) {
	for _, test := range []struct {
		name      string
		usage     string
		reasoning int64
	}{
		{"missing", `{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8},"total_tokens":15}`, 0},
		{"explicit_zero", `{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":0},"total_tokens":15}`, 0},
		{"nonzero", `{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":8},"completion_tokens_details":{"reasoning_tokens":3},"total_tokens":15}`, 3},
		{"no_usage", `null`, 0},
	} {
		for _, streaming := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, streaming), func(t *testing.T) {
				var response []byte
				if streaming {
					converter := NewResponsesStreamConverter("m", nil, nil)
					chunk := fmt.Sprintf(`{"id":"chatcmpl-usage","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}],"usage":%s}`, test.usage)
					if _, err := converter.Feed([]byte(chunk)); err != nil {
						t.Fatal(err)
					}
					frames, err := converter.Done()
					if err != nil {
						t.Fatal(err)
					}
					for _, frame := range frames {
						if !strings.HasPrefix(string(frame), "event: response.completed\n") {
							continue
						}
						_, data, ok := strings.Cut(string(frame), "\ndata: ")
						if !ok {
							t.Fatalf("completed frame has no data: %s", frame)
						}
						var event struct {
							Response json.RawMessage `json:"response"`
						}
						if err := json.Unmarshal([]byte(data), &event); err != nil {
							t.Fatal(err)
						}
						response = event.Response
					}
					if response == nil {
						t.Fatal("response.completed event missing")
					}
				} else {
					completion := fmt.Sprintf(`{"id":"chatcmpl-usage","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":%s}`, test.usage)
					var err error
					response, err = OpenAICompletionToResponses([]byte(completion), "m", nil, nil)
					if err != nil {
						t.Fatal(err)
					}
				}

				var parsed struct {
					Usage *struct {
						InputTokens  int64 `json:"input_tokens"`
						OutputTokens int64 `json:"output_tokens"`
						TotalTokens  int64 `json:"total_tokens"`
						InputDetails struct {
							CachedTokens int64 `json:"cached_tokens"`
						} `json:"input_tokens_details"`
						OutputDetails struct {
							ReasoningTokens *int64 `json:"reasoning_tokens"`
						} `json:"output_tokens_details"`
					} `json:"usage"`
				}
				if err := json.Unmarshal(response, &parsed); err != nil {
					t.Fatal(err)
				}
				if test.usage == "null" {
					if parsed.Usage != nil {
						t.Fatalf("usage invented when upstream sent none: %s", response)
					}
					return
				}
				usage := parsed.Usage
				if usage == nil || usage.OutputDetails.ReasoningTokens == nil {
					t.Fatalf("missing field reasoning_tokens in output_tokens_details: %s", response)
				}
				if *usage.OutputDetails.ReasoningTokens != test.reasoning {
					t.Fatalf("reasoning_tokens = %d, want %d", *usage.OutputDetails.ReasoningTokens, test.reasoning)
				}
				if usage.InputTokens != 10 || usage.OutputTokens != 5 || usage.TotalTokens != 15 || usage.InputDetails.CachedTokens != 8 {
					t.Fatalf("upstream token counts changed: %s", response)
				}
			})
		}
	}
}
