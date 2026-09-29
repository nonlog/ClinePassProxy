package translate

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// ClaudeStreamConverter converts upstream Chat Completions chunks into native
// Anthropic Messages SSE frames.
//
// This is a port of the converter proven in ClinePassBridge: no frame batching,
// no output buffering, and no truncation.
type ClaudeStreamConverter struct {
	model        string
	messageID    string
	toolNames    map[string]string
	toolNameByID map[string]string
	started      bool
	nextBlock    int64
	openKind     string
	openBlock    int64
	tools        map[int64]*claudeToolAccumulator
	finish       string
	sawTool      bool
	invalidTool  bool
	finalized    bool
	terminal     bool
	usage        map[string]any
}

// claudeToolAccumulator collects streamed tool-call fragments.
type claudeToolAccumulator struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

// NewClaudeStreamConverter builds a converter for one response.
// originalRequest is the raw Anthropic request, used to restore tool names that
// the upstream sanitized.
func NewClaudeStreamConverter(model string, originalRequest []byte) *ClaudeStreamConverter {
	return &ClaudeStreamConverter{
		model:        model,
		toolNames:    claudeToolNameMap(originalRequest),
		toolNameByID: claudeToolNameByID(originalRequest),
		tools:        map[int64]*claudeToolAccumulator{},
		usage:        map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
	}
}

// Start emits message_start. Calling it early lets the client begin its turn
// while the upstream is still warming up.
func (c *ClaudeStreamConverter) Start() ([][]byte, error) {
	var out [][]byte
	if err := c.ensureStarted(map[string]any{}, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *ClaudeStreamConverter) ensureStarted(root map[string]any, out *[][]byte) error {
	if c.started {
		return nil
	}
	c.messageID = models.String(root["id"])
	if c.messageID == "" {
		c.messageID = "msg_" + NewID()
	}
	message := map[string]any{
		"id":            c.messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         c.model,
		"content":       []any{},
		"stop_reason":   nil,
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
	}
	if err := AppendSSE(out, "message_start", map[string]any{"type": "message_start", "message": message}); err != nil {
		return err
	}
	c.started = true
	return nil
}

func (c *ClaudeStreamConverter) closeOpenBlock(out *[][]byte) error {
	if c.openKind == "" {
		return nil
	}
	if err := AppendSSE(out, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": c.openBlock,
	}); err != nil {
		return err
	}
	c.openKind = ""
	c.openBlock = -1
	return nil
}

func (c *ClaudeStreamConverter) ensureOpenBlock(kind string, out *[][]byte) error {
	if c.openKind == kind {
		return nil
	}
	if err := c.closeOpenBlock(out); err != nil {
		return err
	}
	index := c.nextBlock
	c.nextBlock++
	block := map[string]any{"type": "text", "text": ""}
	if kind == "thinking" {
		block = map[string]any{"type": "thinking", "thinking": ""}
	}
	if err := AppendSSE(out, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": block,
	}); err != nil {
		return err
	}
	c.openKind = kind
	c.openBlock = index
	return nil
}

func (c *ClaudeStreamConverter) emitThinking(text string, out *[][]byte) error {
	if text == "" {
		return nil
	}
	if err := c.ensureOpenBlock("thinking", out); err != nil {
		return err
	}
	return AppendSSE(out, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": c.openBlock,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	})
}

func (c *ClaudeStreamConverter) emitText(text string, out *[][]byte) error {
	if text == "" {
		return nil
	}
	if err := c.ensureOpenBlock("text", out); err != nil {
		return err
	}
	return AppendSSE(out, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": c.openBlock,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (c *ClaudeStreamConverter) observeToolCalls(delta map[string]any) {
	for arrayIndex, raw := range models.List(delta["tool_calls"]) {
		call := models.Object(raw)
		if call == nil {
			continue
		}
		index := models.Number(call["index"])
		if _, exists := call["index"]; !exists {
			index = int64(arrayIndex)
		}
		accumulator := c.tools[index]
		if accumulator == nil {
			accumulator = &claudeToolAccumulator{}
			c.tools[index] = accumulator
		}
		if id := models.String(call["id"]); id != "" {
			accumulator.ID = id
		}
		function := models.Object(call["function"])
		if name := models.String(function["name"]); name != "" {
			accumulator.Name = name
		}
		if args := models.String(function["arguments"]); args != "" {
			accumulator.Arguments.WriteString(args)
		}
	}
}

func (c *ClaudeStreamConverter) finalizeBlocks(out *[][]byte) error {
	if c.finalized {
		return nil
	}
	if err := c.closeOpenBlock(out); err != nil {
		return err
	}
	indexes := make([]int, 0, len(c.tools))
	for index := range c.tools {
		indexes = append(indexes, int(index))
	}
	sort.Ints(indexes)
	for _, rawIndex := range indexes {
		tool := c.tools[int64(rawIndex)]
		if tool == nil || (tool.ID == "" && tool.Name == "" && tool.Arguments.Len() == 0) {
			continue
		}
		name := tool.Name
		if declared := c.toolNameByID[tool.ID]; declared != "" {
			name = declared
		}
		if name == "" {
			name = fmt.Sprintf("tool_%d", rawIndex)
		}
		name = claudeMapToolName(c.toolNames, name)
		toolID := claudeSanitizeToolID(tool.ID)
		input, partial, valid := claudeToolInput(tool.Arguments.String())
		if !valid {
			c.invalidTool = true
		}
		index := c.nextBlock
		c.nextBlock++
		if err := AppendSSE(out, "content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": index,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    toolID,
				"name":  name,
				"input": input,
			},
		}); err != nil {
			return err
		}
		if partial != "" {
			if err := AppendSSE(out, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
			}); err != nil {
				return err
			}
		}
		if err := AppendSSE(out, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": index,
		}); err != nil {
			return err
		}
		c.sawTool = true
	}
	c.finalized = true
	return nil
}

func (c *ClaudeStreamConverter) emitTerminal(out *[][]byte) error {
	if c.terminal {
		return nil
	}
	if err := c.finalizeBlocks(out); err != nil {
		return err
	}
	usage := c.usage
	if usage == nil {
		usage = map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}
	}
	if err := AppendSSE(out, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   claudeFinishReason(c.finish, c.sawTool, c.invalidTool),
			"stop_sequence": nil,
		},
		"usage": usage,
	}); err != nil {
		return err
	}
	if err := AppendSSE(out, "message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	c.terminal = true
	return nil
}

// Feed converts one upstream Chat Completions chunk into Claude SSE frames.
func (c *ClaudeStreamConverter) Feed(raw []byte) ([][]byte, error) {
	root, err := models.DecodeObject(raw)
	if err != nil {
		return nil, err
	}
	var out [][]byte
	choices := models.List(root["choices"])
	if len(choices) > 0 {
		if err := c.ensureStarted(root, &out); err != nil {
			return nil, err
		}
		choice := models.Object(choices[0])
		delta := models.Object(choice["delta"])
		for _, reasoning := range claudeReasoningTexts(delta) {
			if err := c.emitThinking(reasoning, &out); err != nil {
				return nil, err
			}
		}
		if content := models.String(delta["content"]); content != "" {
			if err := c.emitText(content, &out); err != nil {
				return nil, err
			}
		}
		if len(models.List(delta["tool_calls"])) > 0 {
			c.observeToolCalls(delta)
		}
		if reason := models.String(choice["finish_reason"]); reason != "" {
			c.finish = reason
			if err := c.finalizeBlocks(&out); err != nil {
				return nil, err
			}
		}
	}
	if usage := models.Object(root["usage"]); usage != nil {
		c.usage = claudeUsageFromOpenAI(usage)
	}
	if c.finish != "" && models.Object(root["usage"]) != nil {
		if err := c.emitTerminal(&out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Done emits the terminal frames when the upstream ended without a usage event.
// It refuses to invent a successful ending: an upstream stream that never
// reported a finish reason is a truncated response, not a completed turn.
func (c *ClaudeStreamConverter) Done() ([][]byte, error) {
	if c.terminal {
		return nil, nil
	}
	if c.finish == "" {
		return nil, ErrUpstreamTruncated
	}
	var out [][]byte
	if !c.started {
		if err := c.ensureStarted(map[string]any{}, &out); err != nil {
			return nil, err
		}
	}
	if err := c.emitTerminal(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Usage returns the Claude-shaped usage block collected so far.
func (c *ClaudeStreamConverter) Usage() map[string]any { return c.usage }

// ErrorFrame renders the Anthropic error event that terminates a stream the
// proxy could not complete.
func (c *ClaudeStreamConverter) ErrorFrame(message string) []byte {
	frame, err := SSEEvent("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
	if err != nil {
		return []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"upstream stream truncated\"}}\n\n")
	}
	return frame
}

// FinishReason returns the upstream finish reason.
func (c *ClaudeStreamConverter) FinishReason() string { return c.finish }

// OpenAICompletionToClaude converts a non-streaming Chat Completions response
// into a complete Anthropic Messages response.
func OpenAICompletionToClaude(body []byte, model string, originalRequest []byte) ([]byte, error) {
	root, err := models.DecodeObject(body)
	if err != nil {
		return nil, err
	}
	names := claudeToolNameMap(originalRequest)
	content := []any{}
	sawTool := false
	invalidTool := false
	finishReason := ""
	choices := models.List(root["choices"])
	if len(choices) > 0 {
		choice := models.Object(choices[0])
		finishReason = models.String(choice["finish_reason"])
		message := models.Object(choice["message"])
		for _, reasoning := range claudeReasoningTexts(message) {
			if reasoning != "" {
				content = append(content, map[string]any{"type": "thinking", "thinking": reasoning})
			}
		}
		switch value := message["content"].(type) {
		case string:
			if value != "" {
				content = append(content, map[string]any{"type": "text", "text": value})
			}
		case []any:
			for _, raw := range value {
				part := models.Object(raw)
				switch models.String(part["type"]) {
				case "text":
					if text := models.String(part["text"]); text != "" {
						content = append(content, map[string]any{"type": "text", "text": text})
					}
				case "thinking":
					if thinking := models.String(part["thinking"]); thinking != "" {
						content = append(content, map[string]any{"type": "thinking", "thinking": thinking})
					}
				}
			}
		}
		for _, raw := range models.List(message["tool_calls"]) {
			call := models.Object(raw)
			function := models.Object(call["function"])
			name := claudeMapToolName(names, models.String(function["name"]))
			input, _, valid := claudeToolInput(models.String(function["arguments"]))
			if !valid {
				invalidTool = true
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    claudeSanitizeToolID(models.String(call["id"])),
				"name":  name,
				"input": input,
			})
			sawTool = true
		}
	}
	messageID := models.String(root["id"])
	if messageID == "" {
		messageID = "msg_" + NewID()
	}
	out := map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   claudeFinishReason(finishReason, sawTool, invalidTool),
		"stop_sequence": nil,
		"usage":         claudeUsageFromOpenAI(root["usage"]),
	}
	return json.Marshal(out)
}

// claudeReasoningTexts collects reasoning text from the known upstream fields.
func claudeReasoningTexts(object map[string]any) []string {
	if object == nil {
		return nil
	}
	for _, key := range []string{"reasoning_content", "reasoning", "reasoning_details"} {
		var out []string
		collectClaudeReasoning(object[key], &out)
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

func collectClaudeReasoning(value any, out *[]string) {
	switch current := value.(type) {
	case string:
		if current != "" {
			*out = append(*out, current)
		}
	case []any:
		for _, item := range current {
			collectClaudeReasoning(item, out)
		}
	case map[string]any:
		if text := models.String(current["text"]); text != "" {
			*out = append(*out, text)
		}
	}
}

// claudeUsageFromOpenAI converts OpenAI usage into the Anthropic usage block,
// keeping prompt/cache accounting intact.
func claudeUsageFromOpenAI(value any) map[string]any {
	usage := models.Object(value)
	prompt := models.Number(usage["prompt_tokens"])
	output := models.Number(usage["completion_tokens"])
	if output == 0 {
		output = models.Number(usage["output_tokens"])
	}
	if prompt == 0 {
		prompt = models.Number(usage["input_tokens"])
	}
	details := models.Object(usage["prompt_tokens_details"])
	if details == nil {
		details = models.Object(usage["input_tokens_details"])
	}
	cached := models.Number(details["cached_tokens"])
	cacheWrite := models.Number(details["cache_write_tokens"])
	if cacheWrite <= 0 {
		cacheWrite = models.Number(details["cache_creation_tokens"])
	}
	input := prompt
	if cached > 0 {
		input -= cached
	}
	if cacheWrite > 0 {
		input -= cacheWrite
	}
	if input < 0 {
		input = 0
	}
	out := map[string]any{"input_tokens": input, "output_tokens": output}
	if cached > 0 {
		out["cache_read_input_tokens"] = cached
	}
	if cacheWrite > 0 {
		out["cache_creation_input_tokens"] = cacheWrite
	}
	return out
}

// claudeFinishReason maps an OpenAI finish reason onto Anthropic stop_reason.
func claudeFinishReason(reason string, sawTool, invalidTool bool) string {
	switch reason {
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	}
	if invalidTool {
		return "max_tokens"
	}
	if sawTool {
		return "tool_use"
	}
	switch reason {
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// claudeToolInput parses tool arguments, reporting the normalized JSON and
// whether the arguments were valid.
func claudeToolInput(arguments string) (map[string]any, string, bool) {
	arguments = strings.TrimSpace(arguments)
	if arguments == "" {
		return map[string]any{}, "", true
	}
	var value any
	if json.Unmarshal([]byte(arguments), &value) != nil {
		return map[string]any{}, "{}", false
	}
	input, ok := value.(map[string]any)
	if !ok {
		return map[string]any{}, "{}", false
	}
	normalized, err := json.Marshal(input)
	if err != nil {
		return map[string]any{}, "{}", false
	}
	return input, string(normalized), true
}

func claudeCanonicalToolName(name string) string {
	return strings.ToLower(strings.TrimLeft(strings.TrimSpace(name), "_"))
}

// claudeToolNameMap indexes the tool names declared by the client so sanitized
// upstream names can be restored.
func claudeToolNameMap(original []byte) map[string]string {
	if len(original) == 0 {
		return nil
	}
	root, err := models.DecodeObject(original)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, raw := range models.List(root["tools"]) {
		tool := models.Object(raw)
		name := strings.TrimSpace(models.String(tool["name"]))
		if name == "" {
			name = strings.TrimSpace(models.String(models.Object(tool["function"])["name"]))
		}
		key := claudeCanonicalToolName(name)
		if key == "" {
			continue
		}
		if _, exists := out[key]; !exists {
			out[key] = name
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// claudeToolNameByID maps the tool_use IDs a client declared onto their original
// names. The upstream may rewrite a sanitized name, so the ID is the reliable
// link back to the client-visible tool.
func claudeToolNameByID(original []byte) map[string]string {
	if len(original) == 0 {
		return nil
	}
	root, err := models.DecodeObject(original)
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, rawMessage := range models.List(root["messages"]) {
		for _, rawBlock := range models.List(models.Object(rawMessage)["content"]) {
			block := models.Object(rawBlock)
			if models.String(block["type"]) != "tool_use" {
				continue
			}
			id := models.String(block["id"])
			name := models.String(block["name"])
			if id == "" || name == "" {
				continue
			}
			out[id] = name
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func claudeMapToolName(names map[string]string, name string) string {
	if mapped := names[claudeCanonicalToolName(name)]; mapped != "" {
		return mapped
	}
	return name
}

func claudeSanitizeToolID(value string) string {
	if value == "" {
		return "toolu_" + NewID()
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			builder.WriteByte(ch)
			continue
		}
		builder.WriteByte('_')
	}
	if builder.Len() == 0 {
		return "toolu_" + NewID()
	}
	return builder.String()
}
