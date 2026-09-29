package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestTranslationParityFixtures replays the corpus in testdata/parity.
//
// Each fixture pins one request-translation rule that must stay identical to
// CPA v8.0.4. The comparison is on the complete Chat Completions body, so a
// change in message shape fails even when the result would still "look right".
func TestTranslationParityFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "parity", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("parity corpus is empty")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var fixture struct {
				Protocol string          `json:"protocol"`
				Note     string          `json:"note"`
				Request  json.RawMessage `json:"request"`
				Expected json.RawMessage `json:"expected"`
			}
			if err := json.Unmarshal(raw, &fixture); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			switch fixture.Protocol {
			case "claude-messages":
				got, err = ClaudeMessagesToChatCompletions(fixture.Request, "upstream-model", true)
			case "openai-responses":
				got, err = ResponsesToChatCompletions(fixture.Request, "upstream-model", true)
			default:
				t.Fatalf("unknown protocol %q", fixture.Protocol)
			}
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			var want any
			if err := json.Unmarshal(fixture.Expected, &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plainJSON(got), want) {
				actual, _ := json.MarshalIndent(got, "", "  ")
				expected, _ := json.MarshalIndent(want, "", "  ")
				t.Fatalf("translated request differs from the pinned CPA shape\nnote: %s\nactual:\n%s\nexpected:\n%s",
					fixture.Note, actual, expected)
			}
		})
	}
}

// plainJSON round-trips a value through JSON so maps built in Go compare equal
// to the parsed fixture regardless of the concrete numeric types.
func plainJSON(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var out any
	if json.Unmarshal(encoded, &out) != nil {
		return value
	}
	return out
}
