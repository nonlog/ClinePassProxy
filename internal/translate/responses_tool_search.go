package translate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// responsesRequestToolDeclarations shares the same active definitions between
// request translation and response identity restoration. Newly discovered tools
// append after existing direct tools; repeated definitions replace in place.
func responsesRequestToolDeclarations(root map[string]any) []any {
	discovery := false
	for _, raw := range models.List(root["tools"]) {
		if models.String(models.Object(raw)["type"]) == "tool_search" {
			discovery = true
		}
	}
	var out []any
	positions := map[string]int{}
	var walk func([]any, string, bool)
	walk = func(tools []any, namespace string, loaded bool) {
		for _, raw := range tools {
			tool := models.Object(raw)
			kind := strings.TrimSpace(models.String(tool["type"]))
			if kind == "namespace" {
				walk(models.List(tool["tools"]), strings.TrimSpace(models.String(tool["name"])), loaded)
				continue
			}
			if kind != "function" && kind != "custom" && kind != "tool_search" {
				continue
			}
			if discovery && !loaded && models.Bool(tool["defer_loading"]) {
				continue
			}
			definition := CloneMap(tool)
			if namespace != "" {
				definition["namespace"] = namespace
			}
			name := models.String(definition["name"])
			if name == "" {
				name = models.String(models.Object(definition["function"])["name"])
			}
			key := responsesChatToolName(models.String(definition["namespace"]), name)
			if kind == "tool_search" {
				key = "type:tool_search"
			}
			if index, ok := positions[key]; ok {
				out[index] = definition
			} else {
				positions[key] = len(out)
				out = append(out, definition)
			}
		}
	}
	walk(models.List(root["tools"]), "", false)
	for _, raw := range models.List(root["input"]) {
		item := models.Object(raw)
		if kind := models.String(item["type"]); kind == "tool_search_output" || kind == "additional_tools" {
			walk(models.List(item["tools"]), "", true)
		}
	}
	// A user's ordinary function named tool_search must remain an ordinary
	// function. Choose a free Chat name only for the native discovery tool.
	searchName := "tool_search"
	for index := 1; ; index++ {
		if _, exists := positions[searchName]; !exists {
			break
		}
		searchName = fmt.Sprintf("tool_search_%d", index)
	}
	if index, ok := positions["type:tool_search"]; ok {
		models.Object(out[index])["name"] = searchName
	}
	return out
}

func responsesSearchToolName(declarations []any) string {
	for _, raw := range declarations {
		tool := models.Object(raw)
		if models.String(tool["type"]) == "tool_search" {
			return models.String(tool["name"])
		}
	}
	return "tool_search"
}

func validateResponsesToolSearch(root map[string]any, declarations []any) error {
	for _, raw := range declarations {
		tool := models.Object(raw)
		if models.String(tool["type"]) != "tool_search" {
			continue
		}
		if models.String(tool["execution"]) != "client" {
			return fmt.Errorf("tool_search requires execution=client; hosted tool search is not supported by the Cline Chat Completions upstream")
		}
		if models.String(tool["namespace"]) != "" {
			return fmt.Errorf("client tool_search must be a top-level tool")
		}
		if models.String(models.Object(tool["parameters"])["type"]) != "object" {
			return fmt.Errorf("client tool_search requires a parameters schema")
		}
	}
	for _, raw := range models.List(root["input"]) {
		item := models.Object(raw)
		kind := models.String(item["type"])
		if kind != "tool_search_call" && kind != "tool_search_output" {
			continue
		}
		if models.String(item["execution"]) != "client" || responsesCallID(item) == "" {
			return fmt.Errorf("%s requires execution=client and call_id", kind)
		}
		if kind == "tool_search_call" && models.Object(item["arguments"]) == nil {
			return fmt.Errorf("tool_search_call requires an arguments object")
		}
		if kind == "tool_search_output" && models.List(item["tools"]) == nil {
			return fmt.Errorf("tool_search_output requires a tools array")
		}
	}
	return nil
}

func responsesSearchArguments(arguments string) (map[string]any, error) {
	var object map[string]any
	if json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
		// The caller emits response.failed for this sentinel; never manufacture
		// a completed native discovery call from truncated or invalid JSON.
		return nil, fmt.Errorf("%w: invalid tool_search arguments object", ErrUpstreamTruncated)
	}
	return object, nil
}

func responsesSearchCallItem(id, status string, arguments map[string]any) map[string]any {
	return map[string]any{
		"id": "tsc_" + id, "type": "tool_search_call", "execution": "client",
		"call_id": id, "status": status, "arguments": arguments,
	}
}
