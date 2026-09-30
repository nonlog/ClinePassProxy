package serving

import (
	"bytes"

	"github.com/nonlog/ClinePassProxy/internal/admin"
	"github.com/nonlog/ClinePassProxy/internal/models"
)

// completionKinds distinguishes provider text, reasoning and tool data. No
// content is retained: only the presence and timestamp of each kind is stored.
func completionKinds(root map[string]any) []string {
	seen := map[string]bool{}
	for _, rawChoice := range models.List(root["choices"]) {
		delta := models.Object(models.Object(rawChoice)["delta"])
		if models.String(delta["content"]) != "" {
			seen["text"] = true
		}
		if models.String(delta["reasoning_content"]) != "" || models.String(delta["reasoning"]) != "" {
			seen["reasoning"] = true
		}
		if len(models.List(delta["tool_calls"])) > 0 || models.Object(delta["function_call"]) != nil {
			seen["tool"] = true
		}
	}
	var kinds []string
	for _, kind := range []string{"reasoning", "text", "tool"} {
		if seen[kind] {
			kinds = append(kinds, kind)
		}
	}
	return kinds
}

// writtenKinds measures actual converted output, including deltas emitted only
// by Done(). An upstream reasoning chunk is not automatically a visible text
// token, and a tool-only turn has no visible-text TTFT.
func writtenKinds(frame []byte) []string {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		root, err := models.DecodeObject(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		if err != nil {
			continue
		}
		switch models.String(root["type"]) {
		case "content_block_delta":
			delta := models.Object(root["delta"])
			switch models.String(delta["type"]) {
			case "text_delta":
				if models.String(delta["text"]) != "" {
					return []string{"text"}
				}
			case "thinking_delta":
				if models.String(delta["thinking"]) != "" {
					return []string{"reasoning"}
				}
			case "input_json_delta":
				if models.String(delta["partial_json"]) != "" {
					return []string{"tool"}
				}
			}
		case "content_block_start":
			if models.String(models.Object(root["content_block"])["type"]) == "tool_use" {
				return []string{"tool"}
			}
		case "response.output_text.delta":
			if models.String(root["delta"]) != "" {
				return []string{"text"}
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if models.String(root["delta"]) != "" {
				return []string{"reasoning"}
			}
		case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			if models.String(root["delta"]) != "" {
				return []string{"tool"}
			}
		case "response.output_item.added":
			typ := models.String(models.Object(root["item"])["type"])
			if typ == "function_call" || typ == "custom_tool_call" {
				return []string{"tool"}
			}
		default:
			return completionKinds(root)
		}
	}
	return nil
}

func markFlushed(tracker *admin.Tracker, frames [][]byte) {
	tracker.Stage(admin.StageFirstDownstreamFlush)
	for _, frame := range frames {
		for _, kind := range writtenKinds(frame) {
			tracker.Stage("first_" + kind + "_write")
			tracker.LastStage("last_" + kind + "_write")
			tracker.Stage(admin.StageFirstTokenWrite)
		}
	}
}

func (s *Server) snapshotTransport(request *requestContext) {
	if len(request.transport) == 0 {
		return
	}
	record := request.tracker.Record()
	record.Transport = nil
	for _, trace := range request.transport {
		record.Transport = append(record.Transport, trace.Snapshot())
	}
}
