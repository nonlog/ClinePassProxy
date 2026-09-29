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
func responsesInputMessages(value any, namespace string) ([]any, error) {
	if text, ok := value.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	items := models.List(value)
	if items == nil {
		return nil, nil
	}
	messages := make([]any, 0, len(items))
	for _, raw := range items {
		item := models.Object(raw)
		if item == nil {
			continue
		}
		switch strings.TrimSpace(models.String(item["type"])) {
		case "message", "":
			message := responsesMessageItem(item)
			if message != nil {
				messages = append(messages, message)
			}
		case "function_call":
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   models.String(item["call_id"]),
					"type": "function",
					"function": map[string]any{
						"name":      responsesDeclaredToolName(namespace, models.String(item["name"])),
						"arguments": defaultJSON(models.String(item["arguments"])),
					},
				}},
			})
		case "custom_tool_call":
			messages = append(messages, map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   models.String(item["call_id"]),
					"type": "function",
					"function": map[string]any{
						"name":      responsesDeclaredToolName(namespace, models.String(item["name"])),
						"arguments": customInputArguments(item["input"]),
					},
				}},
			})
		case "function_call_output", "custom_tool_call_output":
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": models.String(item["call_id"]),
				"content":      responsesToolOutput(item["output"]),
			})
		case "reasoning":
			// Provider-private reasoning items are replayed as assistant text-free
			// context only when they carry plain text.
			if text := responsesSummaryText(item["summary"]); text != "" {
				messages = append(messages, map[string]any{
					"role":              "assistant",
					"content":           "",
					"reasoning_content": text,
				})
			}
		case "additional_tools":
			// Tool declarations carried inside the input stream are handled by the
			// caller through the top-level tool list.
		default:
			if message := responsesMessageItem(item); message != nil {
				messages = append(messages, message)
			}
		}
	}
	return messages, nil
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
