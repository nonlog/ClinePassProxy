package serving

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/credentials"
	"github.com/nonlog/ClinePassProxy/internal/models"
)

// completionAggregate reconstructs one blocking Chat Completions response from
// Cline's streaming chunks. Cline can reject very small native non-streaming
// requests with "empty response content" when the entire token budget is spent
// on reasoning. Its streaming endpoint still exposes those reasoning chunks
// and a valid finish_reason, so aggregating that stream preserves the original
// max_tokens semantics instead of raising the client's budget.
type completionAggregate struct {
	root      map[string]any
	choices   map[int64]map[string]any
	tools     map[int64]map[int64]map[string]any
	usage     map[string]any
	hasOutput bool
}

func newCompletionAggregate() *completionAggregate {
	return &completionAggregate{
		root:    map[string]any{},
		choices: map[int64]map[string]any{},
		tools:   map[int64]map[int64]map[string]any{},
	}
}

func (c *completionAggregate) observe(raw []byte) error {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return err
	}
	for _, key := range []string{"id", "created", "model", "system_fingerprint", "provider"} {
		if value, ok := root[key]; ok {
			c.root[key] = value
		}
	}
	if usage := models.Object(root["usage"]); usage != nil {
		if c.usage == nil {
			c.usage = map[string]any{}
		}
		mergeAggregateObjects(c.usage, usage, false)
	}

	for _, rawChoice := range models.List(root["choices"]) {
		choice := models.Object(rawChoice)
		index := models.Number(choice["index"])
		dest := c.choices[index]
		if dest == nil {
			dest = map[string]any{
				"index":         index,
				"message":       map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}
			c.choices[index] = dest
		}
		message := models.Object(dest["message"])
		delta := models.Object(choice["delta"])
		for key, value := range delta {
			switch key {
			case "tool_calls":
				if c.tools[index] == nil {
					c.tools[index] = map[int64]map[string]any{}
				}
				for _, rawTool := range models.List(value) {
					toolCall := models.Object(rawTool)
					toolIndex := models.Number(toolCall["index"])
					tool := c.tools[index][toolIndex]
					if tool == nil {
						tool = map[string]any{}
						c.tools[index][toolIndex] = tool
					}
					for toolKey, toolValue := range toolCall {
						if toolKey == "index" {
							continue
						}
						if toolKey == "function" {
							function := models.Object(tool[toolKey])
							if function == nil {
								function = map[string]any{}
								tool[toolKey] = function
							}
							mergeAggregateObjects(function, models.Object(toolValue), true)
						} else if toolValue != nil {
							tool[toolKey] = toolValue
						}
					}
				}
				c.hasOutput = true
			case "content", "reasoning_content", "reasoning", "refusal":
				if text := models.String(value); text != "" {
					message[key] = models.String(message[key]) + text
					c.hasOutput = true
				}
			case "function_call":
				function := models.Object(message[key])
				if function == nil {
					function = map[string]any{}
					message[key] = function
				}
				mergeAggregateObjects(function, models.Object(value), true)
				c.hasOutput = true
			default:
				if value == nil {
					continue
				}
				if object := models.Object(value); object != nil {
					existing := models.Object(message[key])
					if existing == nil {
						existing = map[string]any{}
						message[key] = existing
					}
					mergeAggregateObjects(existing, object, false)
				} else if array, ok := value.([]any); ok {
					message[key] = append(models.List(message[key]), array...)
				} else {
					message[key] = value
				}
			}
		}
		if choice["finish_reason"] != nil {
			dest["finish_reason"] = choice["finish_reason"]
		}
		if choice["logprobs"] != nil {
			logprobs := models.Object(dest["logprobs"])
			if logprobs == nil {
				logprobs = map[string]any{}
				dest["logprobs"] = logprobs
			}
			for key, value := range models.Object(choice["logprobs"]) {
				if array, ok := value.([]any); ok {
					logprobs[key] = append(models.List(logprobs[key]), array...)
				} else if value != nil {
					logprobs[key] = value
				}
			}
		}
	}
	return nil
}

func (c *completionAggregate) allFinished() bool {
	if len(c.choices) == 0 {
		return false
	}
	for _, choice := range c.choices {
		if choice["finish_reason"] == nil {
			return false
		}
	}
	return true
}

func (c *completionAggregate) result(model string, sawDone bool) ([]byte, error) {
	if !sawDone || !c.allFinished() {
		return nil, errors.New("upstream stream ended before a completion and [DONE]")
	}
	if !c.hasOutput {
		return nil, errors.New("empty response content")
	}

	out := c.root
	out["object"] = "chat.completion"
	out["model"] = model

	indices := make([]int, 0, len(c.choices))
	for index := range c.choices {
		indices = append(indices, int(index))
	}
	sort.Ints(indices)
	choices := make([]any, 0, len(indices))
	for _, rawIndex := range indices {
		index := int64(rawIndex)
		choice := c.choices[index]
		if tools := c.tools[index]; len(tools) > 0 {
			toolIndices := make([]int, 0, len(tools))
			for toolIndex := range tools {
				toolIndices = append(toolIndices, int(toolIndex))
			}
			sort.Ints(toolIndices)
			ordered := make([]any, 0, len(toolIndices))
			for _, rawToolIndex := range toolIndices {
				ordered = append(ordered, tools[int64(rawToolIndex)])
			}
			models.Object(choice["message"])["tool_calls"] = ordered
		}
		choices = append(choices, choice)
	}
	out["choices"] = choices
	if c.usage != nil {
		out["usage"] = c.usage
	}
	return json.Marshal(out)
}

func mergeAggregateObjects(dst, src map[string]any, concatenateStrings bool) {
	for key, value := range src {
		if object := models.Object(value); object != nil {
			existing := models.Object(dst[key])
			if existing == nil {
				existing = map[string]any{}
				dst[key] = existing
			}
			mergeAggregateObjects(existing, object, concatenateStrings)
		} else if concatenateStrings {
			if text, ok := value.(string); ok {
				dst[key] = models.String(dst[key]) + text
			} else if value != nil {
				dst[key] = value
			}
		} else if value != nil {
			dst[key] = value
		}
	}
}

// readNonstreamCompletion first uses Cline's native blocking endpoint. A known
// Cline edge case returns HTTP 500 "empty response content" when a small output
// budget is consumed entirely by reasoning. In that one case retry the exact
// same request as SSE and aggregate it back to a blocking Chat Completion.
//
// The retry does not increase max_tokens and therefore does not change the
// caller's token budget. This mirrors the proven ClinePassBridge behavior while
// keeping native non-streaming as the fast path for normal requests.
func (s *Server) readNonstreamCompletion(
	r *http.Request,
	request *requestContext,
	credential credentials.Record,
	payload map[string]any,
	timeout time.Duration,
) ([]byte, int, error, bool) {
	stream, err := s.openUpstream(r, request, credential, payload, false, timeout)
	if err != nil {
		return nil, http.StatusBadGateway, err, true
	}
	request.tracker.Stage(admin.StageUpstreamHeaders)

	if stream.StatusCode >= 200 && stream.StatusCode < 300 {
		defer stream.Close()
		raw, readErr := readAll(stream, s.limitBytes())
		if readErr != nil {
			return nil, http.StatusBadGateway, readErr, true
		}
		return unwrapCompletionEnvelope(raw), http.StatusOK, nil, false
	}

	status, upstreamErr := upstreamError(stream, "Cline")
	stream.Close()
	if !isEmptyContentError(status, upstreamErr) {
		return nil, status, upstreamErr, retryableUpstreamStatus(status)
	}

	fallback, err := s.openUpstream(r, request, credential, payload, true, timeout)
	if err != nil {
		return nil, http.StatusBadGateway, err, true
	}
	defer fallback.Close()
	request.tracker.Stage("upstream_headers_received")
	if fallback.StatusCode < 200 || fallback.StatusCode >= 300 {
		status, err := upstreamError(fallback, "Cline")
		return nil, status, err, retryableUpstreamStatus(status)
	}

	aggregate := newCompletionAggregate()
	sawDone, err := s.consumeUpstream(
		r,
		request,
		fallback,
		func(raw []byte) ([][]byte, error) {
			if err := aggregate.observe(raw); err != nil {
				return nil, err
			}
			return nil, nil
		},
		func([][]byte) error { return nil },
		nil,
	)
	if err != nil {
		return nil, http.StatusBadGateway, err, false
	}
	raw, err := aggregate.result(request.model, sawDone)
	if err != nil {
		return nil, http.StatusBadGateway, err, false
	}
	return raw, http.StatusOK, nil, false
}

func isEmptyContentError(status int, err error) bool {
	return status == http.StatusInternalServerError &&
		err != nil &&
		strings.Contains(strings.ToLower(err.Error()), "empty response content")
}
