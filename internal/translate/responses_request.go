package translate

import (
	"encoding/json"
	"strings"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// ResponsesToChatCompletions converts an OpenAI Responses request into the
// Chat Completions request that Cline accepts.
func ResponsesToChatCompletions(body []byte, upstreamModel string, stream bool) (map[string]any, error) {
	root, err := models.DecodeObject(body)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"model":    upstreamModel,
		"messages": []any{},
		"stream":   stream,
	}
	if maxTokens, ok := root["max_output_tokens"]; ok {
		out["max_tokens"] = maxTokens
	}
	if temperature, ok := root["temperature"]; ok {
		out["temperature"] = temperature
	}
	if topP, ok := root["top_p"]; ok {
		out["top_p"] = topP
	}
	if parallel, ok := root["parallel_tool_calls"]; ok {
		out["parallel_tool_calls"] = parallel
	}
	if user := models.String(root["user"]); user != "" {
		out["user"] = user
	}
	if effort := responsesReasoningEffort(root["reasoning"]); effort != "" {
		out["reasoning_effort"] = effort
	}

	messages := make([]any, 0, 8)
	if instructions := responsesInstructionsText(root["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}
	input, err := responsesInputMessages(root["input"], "")
	if err != nil {
		return nil, err
	}
	messages = append(messages, input...)
	if len(messages) == 0 {
		return nil, errEmptyConversation{}
	}
	out["messages"] = messages

	if tools := responsesTools(root["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice, ok := responsesToolChoice(root["tool_choice"]); ok {
		out["tool_choice"] = choice
	}
	if format := responsesStructuredFormat(root["text"]); format != nil {
		out["response_format"] = format
	}
	return out, nil
}

// responsesInstructionsText flattens the Responses `instructions` field.
func responsesInstructionsText(value any) string {
	switch content := value.(type) {
	case string:
		return content
	case []any:
		parts := make([]string, 0, len(content))
		for _, raw := range content {
			part := models.Object(raw)
			if text := models.String(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// responsesReasoningEffort reads `reasoning.effort` and `reasoning_effort`.
func responsesReasoningEffort(value any) string {
	if effort := NormalizeEffort(models.String(value)); effort != "" {
		return effort
	}
	if object := models.Object(value); object != nil {
		return NormalizeEffort(models.String(object["effort"]))
	}
	return ""
}

// responsesInputMessages converts the Responses `input` value into Chat
// Completions messages.
//
// The Responses API sends a model turn as a flat list (reasoning, message,
// function_call, function_call_output, ...). Chat Completions requires each
// `tool` message to directly follow the assistant message that carries the
// matching `tool_calls`, and requires parallel calls from one turn to live in a
// single assistant message. Rebuilding that shape is what keeps the upstream
// prompt identical to the turn the provider produced, which is what the prompt
// cache is keyed on.
func responsesInputMessages(value any, namespace string) ([]any, error) {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	items := models.List(value)
	if items == nil {
		return nil, nil
	}
	units := make([]responsesUnit, 0, len(items))
	for _, raw := range items {
		item := models.Object(raw)
		if item == nil {
			continue
		}
		switch strings.TrimSpace(models.String(item["type"])) {
		case "function_call":
			units = append(units, responsesUnit{
				kind:   unitToolCall,
				callID: strings.TrimSpace(models.String(item["call_id"])),
				call: responsesToolCall(
					models.String(item["call_id"]),
					responsesDeclaredToolName(namespace, models.String(item["name"])),
					defaultJSON(models.String(item["arguments"])),
				),
			})
		case "custom_tool_call":
			units = append(units, responsesUnit{
				kind:   unitToolCall,
				callID: strings.TrimSpace(models.String(item["call_id"])),
				call: responsesToolCall(
					models.String(item["call_id"]),
					responsesDeclaredToolName(namespace, models.String(item["name"])),
					customInputArguments(item["input"]),
				),
			})
		case "function_call_output", "custom_tool_call_output":
			units = append(units, responsesUnit{
				kind:   unitToolOutput,
				callID: strings.TrimSpace(models.String(item["call_id"])),
				output: map[string]any{
					"role":    "tool",
					"content": responsesToolOutput(item["output"]),
				},
			})
		case "reasoning":
			// Provider-private reasoning items are replayed only as the plain text
			// they carry; encrypted content is not portable.
			if text := responsesSummaryText(item["summary"]); text != "" {
				units = append(units, responsesUnit{kind: unitReasoning, text: text})
			}
		case "additional_tools":
			// Tool declarations carried inside the input stream are handled by the
			// caller through the top-level tool list.
		default:
			if message := responsesMessageItem(item); message != nil {
				units = append(units, responsesUnit{kind: unitMessage, message: message})
			}
		}
	}
	return responsesAssemble(units), nil
}

// Unit kinds for the Responses input walk.
const (
	unitMessage = iota
	unitToolCall
	unitToolOutput
	unitReasoning
)

// responsesUnit is one converted item of the Responses input list.
type responsesUnit struct {
	kind    int
	callID  string
	message map[string]any
	call    map[string]any
	output  map[string]any
	text    string
}

func responsesToolCall(callID, name, arguments string) map[string]any {
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
}

// responsesAssemble rebuilds Chat Completions message order from the flat
// Responses item list.
//
// Consecutive tool calls become one assistant message, and each tool result is
// emitted directly after the assistant message that carries its call. A call
// that has no result in the history is left without one: an incomplete history
// must not be rewritten, because guessing would change the prompt prefix.
func responsesAssemble(units []responsesUnit) []any {
	out := make([]any, 0, len(units))
	pending := make([]any, 0, 4)
	pendingIDs := make([]string, 0, 4)
	recent := make([]string, 0, 4)
	claimed := map[string]bool{}
	reasoning := ""

	flush := func() {
		if len(pending) == 0 {
			if reasoning != "" {
				out = append(out, map[string]any{"role": "assistant", "content": "", "reasoning_content": reasoning})
				reasoning = ""
			}
			return
		}
		calls := append([]any{}, pending...)
		// A text message produced by the same turn keeps its tool calls in one
		// assistant message instead of splitting the turn in two.
		if last, ok := responsesLastAssistant(out); ok {
			if _, hasCalls := last["tool_calls"]; !hasCalls {
				last["tool_calls"] = calls
				if reasoning != "" {
					last["reasoning_content"] = combineResponsesReasoning(models.String(last["reasoning_content"]), reasoning)
				}
				recent = append(recent[:0], pendingIDs...)
				pending = pending[:0]
				pendingIDs = pendingIDs[:0]
				reasoning = ""
				return
			}
		}
		message := map[string]any{"role": "assistant", "content": "", "tool_calls": calls}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		out = append(out, message)
		recent = append(recent[:0], pendingIDs...)
		pending = pending[:0]
		pendingIDs = pendingIDs[:0]
		reasoning = ""
	}

	for _, unit := range units {
		switch unit.kind {
		case unitToolCall:
			if len(pending) == 0 {
				recent = recent[:0]
				claimed = map[string]bool{}
			}
			pending = append(pending, unit.call)
			pendingIDs = append(pendingIDs, unit.callID)
		case unitReasoning:
			reasoning = combineResponsesReasoning(reasoning, unit.text)
		case unitToolOutput:
			flush()
			unit.output["tool_call_id"] = responsesOutputCallID(unit.callID, recent, claimed)
			out = append(out, unit.output)
		default:
			// The assistant text message of a turn carries the reasoning that
			// preceded it.
			if models.String(unit.message["role"]) == "assistant" && reasoning != "" {
				unit.message["reasoning_content"] = combineResponsesReasoning(models.String(unit.message["reasoning_content"]), reasoning)
				reasoning = ""
			}
			flush()
			out = append(out, unit.message)
		}
	}
	flush()
	return out
}

// responsesOutputCallID resolves the call a tool result belongs to. A result
// that arrives without a call id is matched to the first call of the batch it
// follows that has no result yet, in order.
func responsesOutputCallID(callID string, batchIDs []string, claimed map[string]bool) string {
	if callID != "" {
		claimed[callID] = true
		return callID
	}
	for _, id := range batchIDs {
		if id == "" || claimed[id] {
			continue
		}
		claimed[id] = true
		return id
	}
	return ""
}

func responsesLastAssistant(out []any) (map[string]any, bool) {
	if len(out) == 0 {
		return nil, false
	}
	message, ok := out[len(out)-1].(map[string]any)
	if !ok || models.String(message["role"]) != "assistant" {
		return nil, false
	}
	return message, true
}

func combineResponsesReasoning(first, second string) string {
	switch {
	case first == "":
		return second
	case second == "":
		return first
	default:
		return first + "\n\n" + second
	}
}

func responsesMessageItem(item map[string]any) map[string]any {
	role := strings.TrimSpace(models.String(item["role"]))
	if role != "user" && role != "assistant" && role != "system" && role != "developer" {
		return nil
	}
	if role == "developer" || role == "system" {
		role = "system"
	}
	content := responsesContentParts(item["content"])
	if content == nil {
		if text := models.String(item["content"]); text != "" {
			return map[string]any{"role": role, "content": text}
		}
		return nil
	}
	return map[string]any{"role": role, "content": content}
}

func responsesContentParts(value any) []any {
	items := models.List(value)
	if items == nil {
		return nil
	}
	out := make([]any, 0, len(items))
	for _, raw := range items {
		part := models.Object(raw)
		switch strings.TrimSpace(models.String(part["type"])) {
		case "input_text", "output_text", "text":
			if text := models.String(part["text"]); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "input_image":
			url := models.String(part["image_url"])
			if url == "" {
				url = models.String(part["url"])
			}
			if url != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "refusal":
			if text := models.String(part["refusal"]); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func responsesSummaryText(value any) string {
	items := models.List(value)
	if items == nil {
		return models.String(value)
	}
	parts := make([]string, 0, len(items))
	for _, raw := range items {
		if text := models.String(models.Object(raw)["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func responsesToolOutput(value any) string {
	switch output := value.(type) {
	case string:
		return output
	case nil:
		return ""
	default:
		if encoded, err := json.Marshal(output); err == nil {
			return string(encoded)
		}
	}
	return ""
}

// responsesTools flattens Responses tool declarations into Chat Completions
// function tools, preserving namespace identity in the flattened name so the
// response converter can restore it.
func responsesTools(value any) []any {
	declarations := models.List(value)
	if declarations == nil {
		return nil
	}
	out := make([]any, 0, len(declarations))
	var walk func([]any, string)
	walk = func(tools []any, namespace string) {
		for _, raw := range tools {
			tool := models.Object(raw)
			if tool == nil {
				continue
			}
			switch strings.TrimSpace(models.String(tool["type"])) {
			case "namespace":
				walk(models.List(tool["tools"]), strings.TrimSpace(models.String(tool["name"])))
			case "function":
				name := strings.TrimSpace(models.String(tool["name"]))
				if name == "" {
					name = strings.TrimSpace(models.String(models.Object(tool["function"])["name"]))
				}
				function := models.Object(tool["function"])
				if function == nil {
					function = tool
				}
				parameters := function["parameters"]
				if parameters == nil {
					parameters = function["input_schema"]
				}
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        responsesChatToolName(namespace, name),
						"description": models.String(function["description"]),
						"parameters":  parameters,
					},
				})
			case "custom":
				name := responsesChatToolName(namespace, models.String(tool["name"]))
				if name == "" {
					continue
				}
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        responsesChatToolName(namespace, responsesCustomToolName(name)),
						"description": responsesCustomToolDescription(tool),
						"parameters": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"input": map[string]any{"type": "string", "description": "Raw tool input."},
							},
							"required": []any{"input"},
						},
					},
				})
			default:
				// Built-in server tools (web_search, file_search, ...) have no
				// Chat Completions equivalent and are intentionally dropped.
			}
		}
	}
	walk(declarations, "")
	return out
}

func responsesDeclaredToolName(namespace, name string) string {
	return responsesChatToolName(namespace, name)
}

func responsesCustomToolName(name string) string {
	return name
}

func responsesCustomToolDescription(tool map[string]any) string {
	if description := models.String(tool["description"]); description != "" {
		return description
	}
	return "Freeform tool. Pass the exact tool input as the `input` string argument."
}

func customInputArguments(value any) string {
	switch input := value.(type) {
	case string:
		encoded, err := json.Marshal(map[string]any{"input": input})
		if err != nil {
			return "{}"
		}
		return string(encoded)
	case nil:
		return "{}"
	default:
		encoded, err := json.Marshal(map[string]any{"input": input})
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

// responsesToolChoice converts a Responses tool_choice into Chat Completions form.
func responsesToolChoice(value any) (any, bool) {
	switch choice := value.(type) {
	case string:
		switch strings.TrimSpace(choice) {
		case "auto", "none", "required":
			return strings.TrimSpace(choice), true
		}
		return nil, false
	case map[string]any:
		switch strings.TrimSpace(models.String(choice["type"])) {
		case "function":
			name := strings.TrimSpace(models.String(choice["name"]))
			if name == "" {
				name = strings.TrimSpace(models.String(models.Object(choice["function"])["name"]))
			}
			if name == "" {
				return nil, false
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
		case "custom":
			name := strings.TrimSpace(models.String(choice["name"]))
			if name == "" {
				return nil, false
			}
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}, true
		}
	}
	return nil, false
}

// responsesStructuredFormat maps `text.format` onto `response_format`.
func responsesStructuredFormat(value any) map[string]any {
	text := models.Object(value)
	if text == nil {
		return nil
	}
	format := models.Object(text["format"])
	if format == nil {
		return nil
	}
	switch strings.TrimSpace(models.String(format["type"])) {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		schema := format["schema"]
		if schema == nil {
			schema = models.Object(format["json_schema"])["schema"]
		}
		entry := map[string]any{"name": defaultString(models.String(format["name"]), "response")}
		if schema != nil {
			entry["schema"] = schema
		}
		if strict, ok := format["strict"]; ok {
			entry["strict"] = strict
		}
		return map[string]any{"type": "json_schema", "json_schema": entry}
	}
	return nil
}

func defaultJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
