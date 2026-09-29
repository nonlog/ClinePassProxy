package translate

import (
	"encoding/json"
	"strings"
	"unicode"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// claudeAttributionPrefix marks the Claude Code billing/attribution system block.
// It is removed before translation, matching CPA v8.0.4, so the upstream prompt
// prefix (and therefore the prompt cache key) is unchanged by the migration.
const claudeAttributionPrefix = "x-anthropic-billing-header:"

const (
	toolResultImagePlaceholder = "[Tool returned image content; the images follow in the next user message.]"
	toolResultImageRelayNotice = "Images returned by the preceding tool call(s):"
)

// ClaudeMessagesToChatCompletions converts an Anthropic Messages request into
// the OpenAI Chat Completions request that Cline accepts.
//
// upstreamModel is the resolved Cline model ID; stream selects SSE upstream.
func ClaudeMessagesToChatCompletions(body []byte, upstreamModel string, stream bool) (map[string]any, error) {
	root, err := models.DecodeObject(body)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":    upstreamModel,
		"messages": []any{},
		"stream":   stream,
	}
	if maxTokens, ok := root["max_tokens"]; ok {
		out["max_tokens"] = maxTokens
	}
	if temperature, ok := root["temperature"]; ok {
		out["temperature"] = temperature
	} else if topP, ok := root["top_p"]; ok {
		out["top_p"] = topP
	}
	if stop := claudeStopSequences(root["stop_sequences"]); len(stop) > 0 {
		out["stop"] = stop
	}
	if effort := claudeReasoningEffort(root); effort != "" {
		out["reasoning_effort"] = effort
	}
	if user := models.String(root["user"]); user != "" {
		out["user"] = user
	}

	messages := make([]any, 0, len(models.List(root["messages"]))+1)
	if system := claudeSystemContent(root["system"]); len(system) > 0 {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}

	// Tool-role messages must directly follow the assistant message carrying the
	// matching tool_calls, so tool results are indexed and re-emitted in place.
	rawMessages := models.List(root["messages"])
	toolMessages := map[int][]any{}
	toolNameByID := map[string]string{}
	for index, raw := range rawMessages {
		message := models.Object(raw)
		if message == nil {
			continue
		}
		for _, block := range models.List(message["content"]) {
			part := models.Object(block)
			if models.String(part["type"]) != "tool_use" {
				continue
			}
			if id := models.String(part["id"]); id != "" {
				toolNameByID[id] = models.String(part["name"])
			}
		}
		if strings.EqualFold(models.String(message["role"]), "tool") {
			if converted := claudeToolRoleMessage(message); converted != nil {
				toolMessages[index] = append(toolMessages[index], converted)
			}
		}
	}

	pendingReminders := []any{}
	for index, raw := range rawMessages {
		message := models.Object(raw)
		if message == nil {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(models.String(message["role"])))
		switch role {
		case "system", "developer":
			if text := claudeSystemText(message["content"]); text != "" {
				reminder := map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "text", "text": systemReminderText(text)}},
				}
				pendingReminders = append(pendingReminders, reminder)
			}
			continue
		case "tool":
			// Handled inline with the originating index.
			if converted := toolMessages[index]; len(converted) > 0 {
				messages = append(messages, converted...)
				delete(toolMessages, index)
			}
			continue
		case "user", "assistant":
		default:
			continue
		}

		converted, toolResults, relayImages, err := claudeMessageToChat(role, message, toolNameByID)
		if err != nil {
			return nil, err
		}
		messages = append(messages, toolResults...)
		delete(toolMessages, index)
		if len(relayImages) > 0 {
			relay := make([]any, 0, len(relayImages)+1)
			relay = append(relay, map[string]any{"type": "text", "text": toolResultImageRelayNotice})
			relay = append(relay, relayImages...)
			if role == "user" && converted != nil {
				existing := models.List(converted["content"])
				converted["content"] = append(relay, existing...)
			} else {
				messages = append(messages, map[string]any{"role": "user", "content": relay})
			}
		}
		if len(pendingReminders) > 0 {
			messages = append(messages, pendingReminders...)
			pendingReminders = nil
		}
		if converted != nil {
			messages = append(messages, converted)
		}
	}
	if len(pendingReminders) > 0 {
		messages = append(messages, pendingReminders...)
	}
	// A trailing tool message that never matched a message index.
	for index := range toolMessages {
		messages = append(messages, toolMessages[index]...)
	}
	if len(messages) == 0 {
		return nil, errEmptyConversation{}
	}
	out["messages"] = messages

	if tools := claudeTools(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := claudeToolChoice(root["tool_choice"]); ok {
		out["tool_choice"] = choice
	}
	if toolChoice := models.Object(root["tool_choice"]); toolChoice != nil && models.Bool(toolChoice["disable_parallel_tool_use"]) {
		out["parallel_tool_calls"] = false
	}
	return out, nil
}

// claudeStopSequences normalizes Anthropic stop_sequences.
func claudeStopSequences(value any) []any {
	switch list := value.(type) {
	case []any:
		out := make([]any, 0, len(list))
		for _, item := range list {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	case string:
		if list != "" {
			return []any{list}
		}
	}
	return nil
}

// claudeReasoningEffort maps Anthropic thinking configuration to a reasoning level.
func claudeReasoningEffort(root map[string]any) string {
	config := models.Object(root["thinking"])
	if config == nil {
		return ""
	}
	switch strings.ToLower(models.String(config["type"])) {
	case "enabled":
		budget, ok := config["budget_tokens"]
		if !ok {
			level, _ := BudgetToLevel(-1)
			return level
		}
		level, valid := BudgetToLevel(models.Number(budget))
		if !valid {
			return ""
		}
		return level
	case "adaptive", "auto":
		if outputConfig := models.Object(root["output_config"]); outputConfig != nil {
			if effort := NormalizeEffort(models.String(outputConfig["effort"])); effort != "" {
				return effort
			}
		}
		return "xhigh"
	case "disabled":
		level, _ := BudgetToLevel(0)
		return level
	}
	return ""
}

// claudeSystemText flattens an Anthropic system value into one text string,
// dropping the Claude Code billing/attribution block.
func claudeSystemText(value any) string {
	switch content := value.(type) {
	case string:
		if content == "" || isClaudeAttributionText(content) {
			return ""
		}
		return content
	case []any:
		parts := make([]string, 0, len(content))
		for _, raw := range content {
			part := models.Object(raw)
			if models.String(part["type"]) != "text" {
				continue
			}
			text := models.String(part["text"])
			if text == "" || isClaudeAttributionText(text) {
				continue
			}
			parts = append(parts, text)
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// claudeSystemContent renders the top-level Anthropic system value the way
// CPA v8.0.4 does: an ordered list of text blocks, with the Claude Code
// billing/attribution block dropped.
//
// Joining the blocks into one string changes the bytes of the prompt prefix,
// and the prompt cache is keyed on those bytes, so the block structure is part
// of the request contract rather than a formatting detail.
func claudeSystemContent(value any) []any {
	switch content := value.(type) {
	case string:
		if content == "" || isClaudeAttributionText(content) {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": content}}
	case []any:
		out := make([]any, 0, len(content))
		for _, raw := range content {
			part := models.Object(raw)
			if models.String(part["type"]) != "text" {
				continue
			}
			text := models.String(part["text"])
			if text == "" || isClaudeAttributionText(text) {
				continue
			}
			out = append(out, map[string]any{"type": "text", "text": text})
		}
		return out
	}
	return nil
}

func isClaudeAttributionText(text string) bool {
	return strings.HasPrefix(strings.TrimLeftFunc(text, unicode.IsSpace), claudeAttributionPrefix)
}

// systemReminderText wraps demoted mid-session system instructions so the
// non-Claude upstream still treats them as directives.
func systemReminderText(text string) string {
	return "<system-reminder>\n" + text + "\n</system-reminder>"
}

// claudeMessageToChat converts one user or assistant message.
func claudeMessageToChat(role string, message map[string]any, toolNameByID map[string]string) (map[string]any, []any, []any, error) {
	content := message["content"]
	if text, ok := content.(string); ok {
		return map[string]any{"role": role, "content": text}, nil, nil, nil
	}
	blocks := models.List(content)
	if blocks == nil {
		return nil, nil, nil, nil
	}

	var (
		contentItems []any
		reasoning    []string
		toolCalls    []any
		toolResults  []any
		relayImages  []any
	)
	for _, raw := range blocks {
		part := models.Object(raw)
		if part == nil {
			continue
		}
		switch models.String(part["type"]) {
		case "text":
			text := models.String(part["text"])
			if text == "" || isClaudeAttributionText(text) {
				continue
			}
			contentItems = append(contentItems, map[string]any{"type": "text", "text": text})
		case "image":
			if item, ok := claudeImagePart(part); ok {
				contentItems = append(contentItems, item)
			}
		case "thinking":
			// Only assistant thinking maps back to reasoning_content, and only
			// when it carries no provider-specific signature: a signature from
			// another provider is not portable and would be rejected upstream.
			if role != "assistant" {
				continue
			}
			if strings.TrimSpace(models.String(part["signature"])) != "" {
				continue
			}
			if text := strings.TrimSpace(models.String(part["thinking"])); text != "" {
				reasoning = append(reasoning, text)
			}
		case "redacted_thinking":
			// Never forwarded: redacted thinking is provider-private.
		case "tool_use":
			if role != "assistant" {
				continue
			}
			toolCalls = append(toolCalls, claudeToolCall(part))
		case "tool_result":
			id := models.String(part["tool_use_id"])
			result := map[string]any{
				"role":         "tool",
				"tool_call_id": id,
			}
			if name := toolNameByID[id]; name != "" {
				result["name"] = name
			}
			text, images, fromArray := claudeToolResultContent(part["content"])
			result["content"] = text
			if fromArray && text == "" && len(images) > 0 {
				result["content"] = toolResultImagePlaceholder
			}
			relayImages = append(relayImages, images...)
			toolResults = append(toolResults, result)
		}
	}

	if role == "assistant" {
		if len(contentItems) == 0 && len(toolCalls) == 0 && len(reasoning) == 0 && len(toolResults) == 0 {
			return nil, nil, nil, nil
		}
		out := map[string]any{"role": "assistant"}
		if len(contentItems) > 0 {
			out["content"] = contentItems
		} else {
			out["content"] = ""
		}
		if len(reasoning) > 0 {
			out["reasoning_content"] = strings.Join(reasoning, "\n\n")
		}
		if len(toolCalls) > 0 {
			out["tool_calls"] = toolCalls
		}
		return out, toolResults, relayImages, nil
	}

	out := map[string]any{"role": role}
	switch {
	case len(contentItems) == 1 && models.String(models.Object(contentItems[0])["type"]) == "text":
		out["content"] = models.String(models.Object(contentItems[0])["text"])
	case len(contentItems) > 0:
		out["content"] = contentItems
	default:
		out = nil
	}
	return out, toolResults, relayImages, nil
}

// claudeToolRoleMessage converts a top-level `role: tool` message.
func claudeToolRoleMessage(message map[string]any) map[string]any {
	id := models.String(message["tool_call_id"])
	if id == "" {
		return nil
	}
	result := map[string]any{"role": "tool", "tool_call_id": id}
	if name := models.String(message["name"]); name != "" {
		result["name"] = name
	}
	text, _, _ := claudeToolResultContent(message["content"])
	result["content"] = text
	return result
}

// claudeToolCall converts an Anthropic tool_use block into an OpenAI tool call,
// preserving the original argument bytes so the upstream request is byte-stable.
func claudeToolCall(part map[string]any) map[string]any {
	arguments := json.RawMessage("{}")
	if raw, ok := part["input"]; ok && raw != nil {
		encoded, err := json.Marshal(raw)
		if err == nil {
			arguments = encoded
		}
	}
	return map[string]any{
		"id":   models.String(part["id"]),
		"type": "function",
		"function": map[string]any{
			"name":      models.String(part["name"]),
			"arguments": string(arguments),
		},
	}
}

// claudeImagePart converts an Anthropic image block into an OpenAI content part.
func claudeImagePart(part map[string]any) (map[string]any, bool) {
	url := ""
	if source := models.Object(part["source"]); source != nil {
		switch models.String(source["type"]) {
		case "base64":
			mediaType := models.String(source["media_type"])
			if mediaType == "" {
				mediaType = "application/octet-stream"
			}
			if data := models.String(source["data"]); data != "" {
				url = "data:" + mediaType + ";base64," + data
			}
		case "url":
			url = models.String(source["url"])
		}
	}
	if url == "" {
		url = models.String(part["url"])
	}
	if url == "" {
		return nil, false
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}, true
}

// claudeToolResultContent flattens an Anthropic tool_result content value.
// It returns the text form, image parts extracted for relay, and whether the
// original value was an array.
func claudeToolResultContent(value any) (string, []any, bool) {
	switch content := value.(type) {
	case string:
		return content, nil, false
	case []any:
		parts := make([]string, 0, len(content))
		images := make([]any, 0)
		for _, raw := range content {
			switch item := raw.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				switch models.String(item["type"]) {
				case "text":
					parts = append(parts, models.String(item["text"]))
				case "image":
					if image, ok := claudeImagePart(item); ok {
						images = append(images, image)
					} else if encoded, err := json.Marshal(item); err == nil {
						parts = append(parts, string(encoded))
					}
				default:
					if encoded, err := json.Marshal(item); err == nil {
						parts = append(parts, string(encoded))
					}
				}
			default:
				if encoded, err := json.Marshal(item); err == nil {
					parts = append(parts, string(encoded))
				}
			}
		}
		return strings.Join(parts, "\n\n"), images, true
	case map[string]any:
		if text := models.String(content["text"]); text != "" {
			return text, nil, false
		}
		if encoded, err := json.Marshal(content); err == nil {
			return string(encoded), nil, false
		}
	}
	if value == nil {
		return "", nil, false
	}
	if encoded, err := json.Marshal(value); err == nil {
		return string(encoded), nil, false
	}
	return "", nil, false
}

// claudeTools converts Anthropic tool declarations into OpenAI function tools.
func claudeTools(value any) []any {
	declarations := models.List(value)
	if len(declarations) == 0 {
		return nil
	}
	out := make([]any, 0, len(declarations))
	for _, raw := range declarations {
		tool := models.Object(raw)
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(models.String(tool["name"]))
		if name == "" {
			continue
		}
		function := map[string]any{"name": name}
		if description := models.String(tool["description"]); description != "" {
			function["description"] = description
		}
		if schema := models.Object(tool["input_schema"]); schema != nil {
			function["parameters"] = normalizeSchema(schema)
		} else {
			function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out
}

// claudeToolChoice converts Anthropic tool_choice into OpenAI form.
func claudeToolChoice(value any) (any, bool) {
	if value == nil {
		return nil, false
	}
	if choice, ok := value.(string); ok {
		switch strings.TrimSpace(choice) {
		case "auto":
			return "auto", true
		case "any":
			return "required", true
		case "none":
			return "none", true
		default:
			return "none", true
		}
	}
	choice := models.Object(value)
	if choice == nil {
		return "none", true
	}
	switch models.String(choice["type"]) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		name := strings.TrimSpace(models.String(choice["name"]))
		if name == "" {
			return "none", true
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
	default:
		return "none", true
	}
}

// normalizeSchema guarantees an object schema declares a properties map, which
// some OpenAI-compatible validators require.
func normalizeSchema(schema map[string]any) map[string]any {
	if models.String(schema["type"]) == "object" {
		if _, ok := schema["properties"]; !ok {
			schema["properties"] = map[string]any{}
		}
	}
	return schema
}

type errEmptyConversation struct{}

func (errEmptyConversation) Error() string { return "messages must contain at least one message" }
