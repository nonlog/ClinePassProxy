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
	declarations := responsesRequestToolDeclarations(root)
	if err := validateResponsesToolSearch(root, declarations); err != nil {
		return nil, err
	}
	searchName := responsesSearchToolName(declarations)
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
	input, err := responsesInputMessages(root["input"], "", searchName)
	if err != nil {
		return nil, err
	}
	messages = append(messages, input...)
	if len(messages) == 0 {
		return nil, errEmptyConversation{}
	}
	out["messages"] = messages

	if tools := responsesTools(declarations); len(tools) > 0 {
		out["tools"] = tools
	}
	if models.String(models.Object(root["tool_choice"])["type"]) == "tool_search" {
		out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": searchName}}
	} else if choice, ok := responsesToolChoice(root["tool_choice"]); ok {
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
func responsesInputMessages(value any, namespace, searchName string) ([]any, error) {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	items := models.List(value)
	if items == nil {
		return nil, nil
	}

	// Match CPA v8.0.4's ambiguity guard. A missing call_id is only inferred
	// when there is exactly one unclaimed call/output relationship. With
	// multiple missing outputs, or one missing output facing multiple unclaimed
	// calls, guessing would rewrite the prompt history. Those outputs must stay
	// standalone user content instead.
	explicitOutputCounts := map[string]int{}
	callIDs := map[string]bool{}
	missingOutputIDs := 0
	for _, raw := range items {
		item := models.Object(raw)
		if item == nil {
			continue
		}
		switch strings.TrimSpace(models.String(item["type"])) {
		case "function_call", "custom_tool_call", "tool_search_call":
			if id := responsesCallID(item); id != "" {
				callIDs[id] = true
			}
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			if id := responsesCallID(item); id != "" {
				explicitOutputCounts[id]++
			} else {
				missingOutputIDs++
			}
		}
	}
	unclaimedCalls := 0
	for id := range callIDs {
		if explicitOutputCounts[id] == 0 {
			unclaimedCalls++
		}
	}
	ambiguousMissingOutputs := missingOutputIDs > 1 || (missingOutputIDs > 0 && unclaimedCalls > 1)

	units := make([]responsesUnit, 0, len(items))
	for _, raw := range items {
		item := models.Object(raw)
		if item == nil {
			continue
		}
		switch strings.TrimSpace(models.String(item["type"])) {
		case "tool_search_call":
			callID := responsesCallID(item)
			units = append(units, responsesUnit{
				kind: unitToolCall, callID: callID,
				call: responsesToolCall(callID, searchName, StableJSON(item["arguments"])),
			})
		case "tool_search_output":
			loaded := responsesTools(item["tools"])
			units = append(units, responsesUnit{
				kind: unitToolOutput, callID: responsesCallID(item),
				output: map[string]any{"role": "tool", "content": StableJSON(map[string]any{"tools": loaded})},
			})
		case "function_call":
			callID := responsesCallID(item)
			itemNamespace := strings.TrimSpace(models.String(item["namespace"]))
			if itemNamespace == "" {
				itemNamespace = namespace
			}
			units = append(units, responsesUnit{
				kind:   unitToolCall,
				callID: callID,
				call: responsesToolCall(
					callID,
					responsesDeclaredToolName(itemNamespace, models.String(item["name"])),
					defaultJSON(models.String(item["arguments"])),
				),
			})
		case "custom_tool_call":
			callID := responsesCallID(item)
			itemNamespace := strings.TrimSpace(models.String(item["namespace"]))
			if itemNamespace == "" {
				itemNamespace = namespace
			}
			units = append(units, responsesUnit{
				kind:   unitToolCall,
				callID: callID,
				call: responsesToolCall(
					callID,
					responsesDeclaredToolName(itemNamespace, models.String(item["name"])),
					customInputArguments(item["input"]),
				),
			})
		case "function_call_output", "custom_tool_call_output":
			units = append(units, responsesUnit{
				kind:   unitToolOutput,
				callID: responsesCallID(item),
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
	return responsesAssemble(units, ambiguousMissingOutputs), nil
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
func responsesAssemble(units []responsesUnit, ambiguousMissingOutputs bool) []any {
	out := make([]any, 0, len(units))
	pending := make([]any, 0, 4)
	pendingIDs := make([]string, 0, 4)
	recent := make([]string, 0, 4)
	claimed := map[string]bool{}
	awaiting := map[string]bool{}
	outputCounts := map[string]int{}
	ambiguousIDs := map[string]bool{}
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
				for _, id := range pendingIDs {
					if id != "" {
						awaiting[id] = true
					}
				}
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
		for _, id := range pendingIDs {
			if id != "" {
				awaiting[id] = true
			}
		}
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
			if unit.callID != "" {
				outputCounts[unit.callID]++
				if outputCounts[unit.callID] > 1 {
					ambiguousIDs[unit.callID] = true
				}
			}
			resolvedID := ""
			if !(unit.callID == "" && ambiguousMissingOutputs) {
				resolvedID = responsesOutputCallID(unit.callID, recent, claimed)
			}
			if resolvedID == "" || !awaiting[resolvedID] {
				if content := strings.TrimSpace(models.String(unit.output["content"])); content != "" {
					out = append(out, map[string]any{"role": "user", "content": content})
				}
				continue
			}
			unit.output["tool_call_id"] = resolvedID
			delete(awaiting, resolvedID)
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
	return alignResponsesToolMessages(out, ambiguousIDs)
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

func responsesCallID(item map[string]any) string {
	for _, key := range []string{"call_id", "tool_call_id", "callId"} {
		if id := strings.TrimSpace(models.String(item[key])); id != "" {
			return id
		}
	}
	id := strings.TrimSpace(models.String(item["id"]))
	if strings.HasPrefix(id, "fco_") {
		return ""
	}
	return id
}

func alignResponsesToolMessages(messages []any, extraAmbiguous map[string]bool) []any {
	if len(messages) <= 1 {
		return messages
	}
	type assistantRecord struct {
		index   int
		callIDs []string
		invalid bool
	}
	assistants := make([]assistantRecord, 0)
	assistantByID := map[string]int{}
	ambiguous := map[string]bool{}
	for id := range extraAmbiguous {
		if strings.TrimSpace(id) != "" {
			ambiguous[id] = true
		}
	}
	toolIndices := map[string][]int{}
	for index, raw := range messages {
		message := models.Object(raw)
		switch models.String(message["role"]) {
		case "assistant":
			calls := models.List(message["tool_calls"])
			if len(calls) == 0 {
				continue
			}
			record := assistantRecord{index: index}
			for _, rawCall := range calls {
				id := strings.TrimSpace(models.String(models.Object(rawCall)["id"]))
				if id == "" {
					record.invalid = true
					continue
				}
				if _, exists := assistantByID[id]; exists {
					ambiguous[id] = true
				}
				assistantByID[id] = index
				record.callIDs = append(record.callIDs, id)
			}
			assistants = append(assistants, record)
		case "tool":
			id := strings.TrimSpace(models.String(message["tool_call_id"]))
			if id == "" {
				continue
			}
			toolIndices[id] = append(toolIndices[id], index)
			if len(toolIndices[id]) > 1 {
				ambiguous[id] = true
			}
		}
	}
	moved := map[int]bool{}
	insert := map[int][]any{}
	for _, assistant := range assistants {
		if assistant.invalid || len(assistant.callIDs) == 0 {
			continue
		}
		indices := make([]int, 0, len(assistant.callIDs))
		eligible := true
		for _, id := range assistant.callIDs {
			matches := toolIndices[id]
			if ambiguous[id] || len(matches) != 1 || matches[0] <= assistant.index {
				eligible = false
				break
			}
			indices = append(indices, matches[0])
		}
		if !eligible {
			continue
		}
		alreadyAdjacent := true
		for offset, index := range indices {
			if index != assistant.index+offset+1 {
				alreadyAdjacent = false
				break
			}
		}
		if alreadyAdjacent {
			continue
		}
		for _, index := range indices {
			moved[index] = true
			insert[assistant.index] = append(insert[assistant.index], messages[index])
		}
	}
	if len(moved) == 0 {
		return messages
	}
	out := make([]any, 0, len(messages))
	for index, message := range messages {
		if moved[index] {
			continue
		}
		out = append(out, message)
		out = append(out, insert[index]...)
	}
	return out
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
	if role == "developer" {
		role = "user"
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
			toolNamespace := namespace
			if declared := models.String(tool["namespace"]); declared != "" {
				toolNamespace = declared
			}
			switch strings.TrimSpace(models.String(tool["type"])) {
			case "namespace":
				walk(models.List(tool["tools"]), strings.TrimSpace(models.String(tool["name"])))
			case "tool_search":
				out = append(out, map[string]any{
					"type": "function", "function": map[string]any{
						"name": models.String(tool["name"]), "description": models.String(tool["description"]), "parameters": tool["parameters"],
					},
				})
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
						"name":        responsesChatToolName(toolNamespace, name),
						"description": models.String(function["description"]),
						"parameters":  parameters,
					},
				})
			case "custom":
				name := responsesChatToolName(toolNamespace, models.String(tool["name"]))
				if name == "" {
					continue
				}
				out = append(out, map[string]any{
					"type": "function",
					"function": map[string]any{
						"name":        responsesChatToolName(toolNamespace, responsesCustomToolName(name)),
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
