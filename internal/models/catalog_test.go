package models

import "testing"

func TestClinePassPresets(t *testing.T) {
	presets := ClinePassPresets()
	if len(presets) != 12 {
		t.Fatalf("preset count = %d, want 12", len(presets))
	}
	if presets[0].ID != "cline-pass/glm-5.3" || presets[4].ID != "cline-pass/deepseek-v4.1-flash" {
		t.Fatalf("unexpected preset order: %#v", presets[:5])
	}
	presets[0].ID = "changed"
	if ClinePassPresets()[0].ID == "changed" {
		t.Fatal("preset catalog leaked mutable storage")
	}
}
