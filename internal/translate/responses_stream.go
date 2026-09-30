package translate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// ResponsesStreamConverter converts upstream Chat Completions chunks into native
// OpenAI Responses SSE frames.
//
// Ported from the converter proven in ClinePassBridge, including namespace and
// custom tool identity restoration.
type ResponsesStreamConverter struct {
	model       string
	request     map[string]any
	toolByChat  map[string]ResponsesToolIdentity
	toolByLocal map[string]ResponsesToolIdentity

	seq             int
	responseID      string
	created         int64
	started         bool
	completed       bool
	nextOutputIndex int
	messages        map[int]*responsesMessageState
	reasoning       *responsesReasoningState
	tools           map[string]*responsesToolState

	finishReason     string
	promptTokens     int64
	cachedTokens     int64
	completionTokens int64
	reasoningTokens  int64
	totalTokens      int64
	usageSeen        bool
}

// ResponsesToolIdentity restores the tool identity a Responses client declared.
type ResponsesToolIdentity struct {
	Name      string
	Namespace string
	Custom    bool
}

type responsesToolState struct {
	ID          string
	Name        string
	Identity    ResponsesToolIdentity
	Arguments   strings.Builder
	OutputIndex int
	Started     bool
	Done        bool
}

type responsesMessageState struct {
	OutputIndex int
	Text        strings.Builder
	Started     bool
	Done        bool
}

type responsesReasoningState struct {
	ID          string
	OutputIndex int
	Text        strings.Builder
	Started     bool
	Done        bool
}

// NewResponsesStreamConverter builds a converter for one response.
func NewResponsesStreamConverter(model string, originalRequest, translatedRequest []byte) *ResponsesStreamConverter {
	var request map[string]any
	_ = json.Unmarshal(originalRequest, &request)
	if request == nil {
		request = map[string]any{}
	}
	byChat, byLocal := buildResponsesToolMap(originalRequest, translatedRequest)
	return &ResponsesStreamConverter{
		model:       model,
		request:     request,
		toolByChat:  byChat,
		toolByLocal: byLocal,
		messages:    map[int]*responsesMessageState{},
		tools:       map[string]*responsesToolState{},
	}
}

// Start emits response.created and response.in_progress.
func (c *ResponsesStreamConverter) Start() ([][]byte, error) {
	var out [][]byte
	if err := c.ensureStarted(map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ResponsesStreamConverter) nextSeq() int {
	c.seq++
	return c.seq
}

func (c *ResponsesStreamConverter) nextOutput() int {
	index := c.nextOutputIndex
	c.nextOutputIndex++
	return index
}

func (c *ResponsesStreamConverter) requestModel() string {
	if model := strings.TrimSpace(models.String(c.request["model"])); model != "" {
		return model
	}
	return c.model
}

func (c *ResponsesStreamConverter) ensureStarted(root map[string]any, out *[][]byte) error {
	if c.started {
		return nil
	}
	c.responseID = models.String(root["id"])
	if c.responseID == "" {
		c.responseID = "resp_" + NewID()
	}
	c.created = models.Number(root["created"])
	if c.created == 0 {
		c.created = time.Now().Unix()
	}
	base := map[string]any{
		"id":         c.responseID,
		"object":     "response",
		"created_at": c.created,
		"status":     "in_progress",
		"background": false,
		"error":      nil,
		"output":     []any{},
	}
	if model := c.requestModel(); model != "" {
		base["model"] = model
	}
	if err := AppendSSE(out, "response.created", map[string]any{
		"type": "response.created", "sequence_number": c.nextSeq(), "response": CloneMap(base),
	}); err != nil {
		return err
	}
	if err := AppendSSE(out, "response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": c.nextSeq(), "response": CloneMap(base),
	}); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *ResponsesStreamConverter) updateUsage(value any) {
	usage := models.Object(value)
	if usage == nil {
		return
	}
	if models.Has(usage, "prompt_tokens") {
		c.promptTokens = models.Number(usage["prompt_tokens"])
		c.usageSeen = true
	} else if models.Has(usage, "input_tokens") {
		c.promptTokens = models.Number(usage["input_tokens"])
		c.usageSeen = true
	}
	details := models.Object(usage["prompt_tokens_details"])
	if details == nil {
		details = models.Object(usage["input_tokens_details"])
	}
	if models.Has(details, "cached_tokens") {
		c.cachedTokens = models.Number(details["cached_tokens"])
		c.usageSeen = true
	}
	if models.Has(usage, "completion_tokens") {
		c.completionTokens = models.Number(usage["completion_tokens"])
		c.usageSeen = true
	} else if models.Has(usage, "output_tokens") {
		c.completionTokens = models.Number(usage["output_tokens"])
		c.usageSeen = true
	}
	for _, key := range []string{"completion_tokens_details", "output_tokens_details"} {
		if detail := models.Object(usage[key]); models.Has(detail, "reasoning_tokens") {
			c.reasoningTokens = models.Number(detail["reasoning_tokens"])
			c.usageSeen = true
		}
	}
	if models.Has(usage, "total_tokens") {
		c.totalTokens = models.Number(usage["total_tokens"])
		c.usageSeen = true
	}
}

func responsesReasoningText(delta map[string]any) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text := models.String(delta[key]); text != "" {
			return text
		}
	}
	return ""
}

func (c *ResponsesStreamConverter) ensureReasoning(choiceIndex int, out *[][]byte) error {
	if c.reasoning != nil && c.reasoning.Started && !c.reasoning.Done {
		return nil
	}
	state := &responsesReasoningState{
		ID:          fmt.Sprintf("rs_%s_%d", c.responseID, choiceIndex),
		OutputIndex: c.nextOutput(),
		Started:     true,
	}
	c.reasoning = state
	if err := AppendSSE(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{"id": state.ID, "type": "reasoning", "status": "in_progress", "summary": []any{}},
	}); err != nil {
		return err
	}
	return AppendSSE(out, "response.reasoning_summary_part.added", map[string]any{
		"type": "response.reasoning_summary_part.added", "sequence_number": c.nextSeq(),
		"item_id": state.ID, "output_index": state.OutputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": ""},
	})
}

func (c *ResponsesStreamConverter) closeReasoning(out *[][]byte) error {
	state := c.reasoning
	if state == nil || !state.Started || state.Done {
		return nil
	}
	text := state.Text.String()
	if err := AppendSSE(out, "response.reasoning_summary_text.done", map[string]any{
		"type": "response.reasoning_summary_text.done", "sequence_number": c.nextSeq(),
		"item_id": state.ID, "output_index": state.OutputIndex, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := AppendSSE(out, "response.reasoning_summary_part.done", map[string]any{
		"type": "response.reasoning_summary_part.done", "sequence_number": c.nextSeq(),
		"item_id": state.ID, "output_index": state.OutputIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	}); err != nil {
		return err
	}
	if err := AppendSSE(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{
			"id": state.ID, "type": "reasoning", "encrypted_content": "",
			"summary": []any{map[string]any{"type": "summary_text", "text": text}},
		},
	}); err != nil {
		return err
	}
	state.Done = true
	return nil
}

func (c *ResponsesStreamConverter) ensureMessage(index int, out *[][]byte) (*responsesMessageState, error) {
	state := c.messages[index]
	if state == nil {
		state = &responsesMessageState{OutputIndex: c.nextOutput()}
		c.messages[index] = state
	}
	if state.Started {
		return state, nil
	}
	messageID := fmt.Sprintf("msg_%s_%d", c.responseID, index)
	if err := AppendSSE(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{"id": messageID, "type": "message", "status": "in_progress", "content": []any{}, "role": "assistant"},
	}); err != nil {
		return nil, err
	}
	if err := AppendSSE(out, "response.content_part.added", map[string]any{
		"type": "response.content_part.added", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": ""},
	}); err != nil {
		return nil, err
	}
	state.Started = true
	return state, nil
}

func responsesIncomplete(finishReason string) (string, map[string]any) {
	switch finishReason {
	case "length", "max_tokens":
		return "incomplete", map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		return "incomplete", map[string]any{"reason": "content_filter"}
	default:
		return "completed", nil
	}
}

func (c *ResponsesStreamConverter) closeMessage(index int, out *[][]byte) error {
	state := c.messages[index]
	if state == nil || !state.Started || state.Done {
		return nil
	}
	messageID := fmt.Sprintf("msg_%s_%d", c.responseID, index)
	text := state.Text.String()
	if err := AppendSSE(out, "response.output_text.done", map[string]any{
		"type": "response.output_text.done", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0, "text": text, "logprobs": []any{},
	}); err != nil {
		return err
	}
	if err := AppendSSE(out, "response.content_part.done", map[string]any{
		"type": "response.content_part.done", "sequence_number": c.nextSeq(), "item_id": messageID,
		"output_index": state.OutputIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text},
	}); err != nil {
		return err
	}
	status, _ := responsesIncomplete(c.finishReason)
	if err := AppendSSE(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex,
		"item": map[string]any{
			"id": messageID, "type": "message", "status": status, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text}},
		},
	}); err != nil {
		return err
	}
	state.Done = true
	return nil
}

func responsesToolKey(choiceIndex, toolIndex int) string {
	return fmt.Sprintf("%d:%d", choiceIndex, toolIndex)
}

func (c *ResponsesStreamConverter) identityForTool(name string) ResponsesToolIdentity {
	if identity, ok := c.toolByChat[name]; ok {
		return identity
	}
	if identity, ok := c.toolByLocal[name]; ok {
		return identity
	}
	return ResponsesToolIdentity{Name: name}
}

func (c *ResponsesStreamConverter) ensureTool(state *responsesToolState, out *[][]byte) error {
	if state.Started {
		return nil
	}
	if state.ID == "" && state.Name == "" {
		return nil
	}
	if state.ID == "" {
		state.ID = "call_" + NewID()
	}
	state.Identity = c.identityForTool(state.Name)
	name := state.Identity.Name
	if name == "" {
		name = state.Name
	}
	itemID, itemType := "fc_"+state.ID, "function_call"
	item := map[string]any{
		"id": itemID, "type": itemType, "status": "in_progress",
		"arguments": "", "call_id": state.ID, "name": name,
	}
	if state.Identity.Custom {
		itemID, itemType = "ctc_"+state.ID, "custom_tool_call"
		item = map[string]any{
			"id": itemID, "type": itemType, "status": "in_progress",
			"input": "", "call_id": state.ID, "name": name,
		}
	}
	if state.Identity.Namespace != "" {
		item["namespace"] = state.Identity.Namespace
	}
	if err := AppendSSE(out, "response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
	}); err != nil {
		return err
	}
	state.Started = true
	return nil
}

func unwrapResponsesCustomInput(arguments string) string {
	var value map[string]any
	if json.Unmarshal([]byte(arguments), &value) == nil {
		if input, ok := value["input"]; ok {
			if text, ok := input.(string); ok {
				return text
			}
			if encoded, err := json.Marshal(input); err == nil {
				return string(encoded)
			}
		}
	}
	return arguments
}

func (c *ResponsesStreamConverter) closeTool(state *responsesToolState, out *[][]byte) error {
	if state.Done {
		return nil
	}
	if err := c.ensureTool(state, out); err != nil {
		return err
	}
	if !state.Started {
		return nil
	}
	args := state.Arguments.String()
	if args == "" {
		args = "{}"
	}
	status, _ := responsesIncomplete(c.finishReason)
	name := state.Identity.Name
	if name == "" {
		name = state.Name
	}
	if state.Identity.Custom {
		input := unwrapResponsesCustomInput(args)
		if err := AppendSSE(out, "response.custom_tool_call_input.done", map[string]any{
			"type": "response.custom_tool_call_input.done", "sequence_number": c.nextSeq(),
			"item_id": "ctc_" + state.ID, "output_index": state.OutputIndex, "input": input,
		}); err != nil {
			return err
		}
		item := map[string]any{
			"id": "ctc_" + state.ID, "type": "custom_tool_call", "status": status,
			"input": input, "call_id": state.ID, "name": name,
		}
		if state.Identity.Namespace != "" {
			item["namespace"] = state.Identity.Namespace
		}
		if err := AppendSSE(out, "response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
		}); err != nil {
			return err
		}
		state.Done = true
		return nil
	}
	if err := AppendSSE(out, "response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "sequence_number": c.nextSeq(),
		"item_id": "fc_" + state.ID, "output_index": state.OutputIndex, "arguments": args,
	}); err != nil {
		return err
	}
	item := map[string]any{
		"id": "fc_" + state.ID, "type": "function_call", "status": status,
		"arguments": args, "call_id": state.ID, "name": name,
	}
	if state.Identity.Namespace != "" {
		item["namespace"] = state.Identity.Namespace
	}
	if err := AppendSSE(out, "response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": c.nextSeq(), "output_index": state.OutputIndex, "item": item,
	}); err != nil {
		return err
	}
	state.Done = true
	return nil
}

func (c *ResponsesStreamConverter) finalizeOpenItems(out *[][]byte) error {
	if err := c.closeReasoning(out); err != nil {
		return err
	}
	indexes := make([]int, 0, len(c.messages))
	for index := range c.messages {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if err := c.closeMessage(index, out); err != nil {
			return err
		}
	}
	keys := make([]string, 0, len(c.tools))
	for key := range c.tools {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return c.tools[keys[i]].OutputIndex < c.tools[keys[j]].OutputIndex })
	for _, key := range keys {
		if err := c.closeTool(c.tools[key], out); err != nil {
			return err
		}
	}
	return nil
}

// Feed converts one upstream Chat Completions chunk into Responses SSE frames.
func (c *ResponsesStreamConverter) Feed(raw []byte) ([][]byte, error) {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	c.updateUsage(root["usage"])
	choices := models.List(root["choices"])
	if len(choices) == 0 {
		return nil, nil
	}
	var out [][]byte
	if err := c.ensureStarted(root, &out); err != nil {
		return nil, err
	}
	for choicePosition, rawChoice := range choices {
		choice := models.Object(rawChoice)
		index := int(models.Number(choice["index"]))
		if _, exists := choice["index"]; !exists {
			index = choicePosition
		}
		delta := models.Object(choice["delta"])
		if delta != nil {
			if reasoning := responsesReasoningText(delta); reasoning != "" {
				if err := c.ensureReasoning(index, &out); err != nil {
					return nil, err
				}
				c.reasoning.Text.WriteString(reasoning)
				if err := AppendSSE(&out, "response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "sequence_number": c.nextSeq(),
					"item_id": c.reasoning.ID, "output_index": c.reasoning.OutputIndex,
					"summary_index": 0, "delta": reasoning,
				}); err != nil {
					return nil, err
				}
			}
			if content := models.String(delta["content"]); content != "" {
				if err := c.closeReasoning(&out); err != nil {
					return nil, err
				}
				message, err := c.ensureMessage(index, &out)
				if err != nil {
					return nil, err
				}
				message.Text.WriteString(content)
				if err := AppendSSE(&out, "response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "sequence_number": c.nextSeq(),
					"item_id":       fmt.Sprintf("msg_%s_%d", c.responseID, index),
					"output_index":  message.OutputIndex,
					"content_index": 0, "delta": content, "logprobs": []any{},
				}); err != nil {
					return nil, err
				}
			}
			for toolPosition, rawTool := range models.List(delta["tool_calls"]) {
				if err := c.closeReasoning(&out); err != nil {
					return nil, err
				}
				if err := c.closeMessage(index, &out); err != nil {
					return nil, err
				}
				tool := models.Object(rawTool)
				toolIndex := int(models.Number(tool["index"]))
				if _, exists := tool["index"]; !exists {
					toolIndex = toolPosition
				}
				key := responsesToolKey(index, toolIndex)
				state := c.tools[key]
				if state == nil {
					state = &responsesToolState{OutputIndex: c.nextOutput()}
					c.tools[key] = state
				}
				if callID := models.String(tool["id"]); callID != "" {
					state.ID = callID
				}
				function := models.Object(tool["function"])
				if name := models.String(function["name"]); name != "" {
					state.Name = name
				}
				argsDelta := models.String(function["arguments"])
				if argsDelta != "" {
					state.Arguments.WriteString(argsDelta)
				}
				if err := c.ensureTool(state, &out); err != nil {
					return nil, err
				}
				if argsDelta != "" && state.Started && !state.Identity.Custom {
					if err := AppendSSE(&out, "response.function_call_arguments.delta", map[string]any{
						"type": "response.function_call_arguments.delta", "sequence_number": c.nextSeq(),
						"item_id": "fc_" + state.ID, "output_index": state.OutputIndex, "delta": argsDelta,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
		if finish := models.String(choice["finish_reason"]); finish != "" {
			c.finishReason = finish
			if err := c.finalizeOpenItems(&out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// Usage returns the OpenAI Responses usage block collected so far.
func (c *ResponsesStreamConverter) Usage() map[string]any {
	if !c.usageSeen {
		return nil
	}
	total := c.totalTokens
	if total == 0 {
		total = c.promptTokens + c.completionTokens
	}
	return map[string]any{
		"input_tokens":          c.promptTokens,
		"input_tokens_details":  map[string]any{"cached_tokens": c.cachedTokens},
		"output_tokens":         c.completionTokens,
		"output_tokens_details": map[string]any{"reasoning_tokens": c.reasoningTokens},
		"total_tokens":          total,
	}
}

// Identifiers of the collected response, for diagnostics.
func (c *ResponsesStreamConverter) ResponseID() string { return c.responseID }

// ErrorFrame renders the Responses `response.failed` event that terminates a
// stream the proxy could not complete.
func (c *ResponsesStreamConverter) ErrorFrame(message string) []byte {
	response := map[string]any{
		"id": c.responseID, "object": "response", "created_at": c.created, "status": "failed",
		"background": false, "error": map[string]any{"code": "upstream_error", "message": message},
		"output": c.completedOutput(),
	}
	if model := c.requestModel(); model != "" {
		response["model"] = model
	}
	frame, err := SSEEvent("response.failed", map[string]any{
		"type": "response.failed", "sequence_number": c.nextSeq(), "response": response,
	})
	if err != nil {
		return []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n")
	}
	return frame
}

func (c *ResponsesStreamConverter) completedOutput() []any {
	type indexed struct {
		index int
		item  map[string]any
	}
	items := make([]indexed, 0, len(c.messages)+len(c.tools)+1)
	if state := c.reasoning; state != nil && state.Started {
		items = append(items, indexed{state.OutputIndex, map[string]any{
			"id": state.ID, "type": "reasoning", "encrypted_content": "",
			"summary": []any{map[string]any{"type": "summary_text", "text": state.Text.String()}},
		}})
	}
	status, _ := responsesIncomplete(c.finishReason)
	for index, state := range c.messages {
		if !state.Started {
			continue
		}
		items = append(items, indexed{state.OutputIndex, map[string]any{
			"id": fmt.Sprintf("msg_%s_%d", c.responseID, index), "type": "message", "status": status, "role": "assistant",
			"content": []any{map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": state.Text.String()}},
		}})
	}
	for _, state := range c.tools {
		if !state.Started {
			continue
		}
		name := state.Identity.Name
		if name == "" {
			name = state.Name
		}
		args := state.Arguments.String()
		if args == "" {
			args = "{}"
		}
		var item map[string]any
		if state.Identity.Custom {
			item = map[string]any{
				"id": "ctc_" + state.ID, "type": "custom_tool_call", "status": status,
				"input": unwrapResponsesCustomInput(args), "call_id": state.ID, "name": name,
			}
		} else {
			item = map[string]any{
				"id": "fc_" + state.ID, "type": "function_call", "status": status,
				"arguments": args, "call_id": state.ID, "name": name,
			}
		}
		if state.Identity.Namespace != "" {
			item["namespace"] = state.Identity.Namespace
		}
		items = append(items, indexed{state.OutputIndex, item})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item.item)
	}
	return out
}

func (c *ResponsesStreamConverter) completedResponse() map[string]any {
	status, incomplete := responsesIncomplete(c.finishReason)
	response := map[string]any{
		"id": c.responseID, "object": "response", "created_at": c.created, "status": status,
		"background": false, "error": nil, "output": c.completedOutput(),
	}
	if incomplete != nil {
		response["incomplete_details"] = incomplete
	} else {
		response["incomplete_details"] = nil
	}
	if model := c.requestModel(); model != "" {
		response["model"] = model
	}
	for _, key := range []string{
		"instructions", "max_output_tokens", "max_tool_calls", "parallel_tool_calls",
		"previous_response_id", "prompt_cache_key", "reasoning", "safety_identifier",
		"service_tier", "store", "temperature", "text", "tool_choice", "tools",
		"top_logprobs", "top_p", "truncation", "user", "metadata",
	} {
		if value, ok := c.request[key]; ok {
			response[key] = value
		}
	}
	if usage := c.Usage(); usage != nil {
		response["usage"] = usage
	}
	return response
}

// Done emits the terminal response.completed or response.incomplete frame. An
// upstream stream that never reported a finish reason did not complete, so it
// is reported as a failure instead of being closed as a healthy response.
func (c *ResponsesStreamConverter) Done() ([][]byte, error) {
	if c.completed {
		return nil, nil
	}
	var out [][]byte
	if !c.started {
		return nil, errResponsesNoOutput
	}
	if c.finishReason == "" {
		return nil, ErrUpstreamTruncated
	}
	if err := c.finalizeOpenItems(&out); err != nil {
		return nil, err
	}
	status, _ := responsesIncomplete(c.finishReason)
	event := "response.completed"
	if status == "incomplete" {
		event = "response.incomplete"
	}
	if err := AppendSSE(&out, event, map[string]any{
		"type": event, "sequence_number": c.nextSeq(), "response": c.completedResponse(),
	}); err != nil {
		return nil, err
	}
	c.completed = true
	return out, nil
}

type errResponsesNoOutputType struct{}

func (errResponsesNoOutputType) Error() string {
	return "upstream stream ended before a response item"
}

var errResponsesNoOutput = errResponsesNoOutputType{}

// OpenAICompletionToResponses converts a non-streaming Chat Completions response
// into a complete OpenAI Responses object.
func OpenAICompletionToResponses(raw []byte, model string, originalRequest, translatedRequest []byte) ([]byte, error) {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	converter := NewResponsesStreamConverter(model, originalRequest, translatedRequest)
	converter.responseID = models.String(root["id"])
	if converter.responseID == "" {
		converter.responseID = "resp_" + NewID()
	}
	converter.created = models.Number(root["created"])
	if converter.created == 0 {
		converter.created = time.Now().Unix()
	}
	converter.started = true
	converter.updateUsage(root["usage"])
	for choicePosition, rawChoice := range models.List(root["choices"]) {
		choice := models.Object(rawChoice)
		index := int(models.Number(choice["index"]))
		if _, exists := choice["index"]; !exists {
			index = choicePosition
		}
		message := models.Object(choice["message"])
		if reasoning := responsesReasoningText(message); reasoning != "" {
			converter.reasoning = &responsesReasoningState{
				ID:          fmt.Sprintf("rs_%s_%d", converter.responseID, index),
				OutputIndex: converter.nextOutput(),
				Started:     true,
				Done:        true,
			}
			converter.reasoning.Text.WriteString(reasoning)
		}
		if content := models.String(message["content"]); content != "" {
			state := &responsesMessageState{OutputIndex: converter.nextOutput(), Started: true, Done: true}
			state.Text.WriteString(content)
			converter.messages[index] = state
		}
		for toolPosition, rawTool := range models.List(message["tool_calls"]) {
			tool := models.Object(rawTool)
			toolIndex := int(models.Number(tool["index"]))
			if _, exists := tool["index"]; !exists {
				toolIndex = toolPosition
			}
			function := models.Object(tool["function"])
			name := models.String(function["name"])
			state := &responsesToolState{
				ID:          models.String(tool["id"]),
				Name:        name,
				Identity:    converter.identityForTool(name),
				OutputIndex: converter.nextOutput(),
				Started:     true,
				Done:        true,
			}
			if state.ID == "" {
				state.ID = "call_" + NewID()
			}
			state.Arguments.WriteString(models.String(function["arguments"]))
			converter.tools[responsesToolKey(index, toolIndex)] = state
		}
		if finish := models.String(choice["finish_reason"]); finish != "" {
			converter.finishReason = finish
		}
	}
	return json.Marshal(converter.completedResponse())
}

// buildResponsesToolMap maps the upstream tool names back onto the tool
// declarations the Responses client sent.
func buildResponsesToolMap(originalRequest, translatedRequest []byte) (map[string]ResponsesToolIdentity, map[string]ResponsesToolIdentity) {
	declarations := collectResponsesToolDeclarations(originalRequest)
	var translated map[string]any
	_ = json.Unmarshal(translatedRequest, &translated)
	chatNames := make([]string, 0)
	for _, rawTool := range models.List(translated["tools"]) {
		if name := models.String(models.Object(models.Object(rawTool)["function"])["name"]); name != "" {
			chatNames = append(chatNames, name)
		}
	}
	byChat := map[string]ResponsesToolIdentity{}
	byLocal := map[string]ResponsesToolIdentity{}
	if len(chatNames) == len(declarations) {
		for i, name := range chatNames {
			byChat[name] = declarations[i]
		}
	} else {
		for _, declaration := range declarations {
			expected := responsesChatToolName(declaration.Namespace, declaration.Name)
			for _, chatName := range chatNames {
				if chatName == expected {
					byChat[chatName] = declaration
					break
				}
			}
		}
	}
	ambiguous := map[string]bool{}
	for _, declaration := range declarations {
		if existing, ok := byLocal[declaration.Name]; ok && (existing.Namespace != declaration.Namespace || existing.Custom != declaration.Custom) {
			ambiguous[declaration.Name] = true
			continue
		}
		byLocal[declaration.Name] = declaration
	}
	for name := range ambiguous {
		delete(byLocal, name)
	}
	return byChat, byLocal
}

func collectResponsesToolDeclarations(raw []byte) []ResponsesToolIdentity {
	var root map[string]any
	_ = json.Unmarshal(raw, &root)
	var out []ResponsesToolIdentity
	var scan func([]any, string)
	scan = func(tools []any, namespace string) {
		for _, rawTool := range tools {
			tool := models.Object(rawTool)
			kind := strings.TrimSpace(models.String(tool["type"]))
			if kind == "namespace" {
				scan(models.List(tool["tools"]), strings.TrimSpace(models.String(tool["name"])))
				continue
			}
			if kind != "" && kind != "function" && kind != "custom" {
				continue
			}
			name := strings.TrimSpace(models.String(tool["name"]))
			if name == "" {
				name = strings.TrimSpace(models.String(models.Object(tool["function"])["name"]))
			}
			if name == "" {
				continue
			}
			out = append(out, ResponsesToolIdentity{Name: name, Namespace: namespace, Custom: kind == "custom"})
		}
	}
	scan(models.List(root["tools"]), "")
	for _, rawItem := range models.List(root["input"]) {
		item := models.Object(rawItem)
		if models.String(item["type"]) == "additional_tools" {
			scan(models.List(item["tools"]), "")
		}
	}
	return out
}

// responsesChatToolName flattens a namespaced Responses tool name into the
// 64-character Chat Completions form.
func responsesChatToolName(namespace, name string) string {
	name = strings.TrimSpace(name)
	namespace = strings.TrimSpace(namespace)
	if namespace != "" && !strings.HasPrefix(name, "mcp__") && !strings.HasPrefix(name, namespace) {
		if strings.HasSuffix(namespace, "__") {
			name = namespace + name
		} else {
			name = namespace + "__" + name
		}
	}
	if len(name) <= 64 {
		return name
	}
	name = name[len(name)-64:]
	if trimmed := strings.TrimLeft(name, "_-"); trimmed != "" {
		return trimmed
	}
	return name
}
