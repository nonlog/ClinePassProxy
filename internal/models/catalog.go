package models

// ClinePassCatalogURL is the official list used to maintain the preset model
// catalog shown by the management console.
const ClinePassCatalogURL = "https://docs.cline.bot/getting-started/clinepass"

// Preset is a documented model that can be imported into the local alias table.
// It is deliberately separate from Entry: presets do not change routing until
// the operator explicitly adds one.
type Preset struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

var clinePassPresets = []Preset{
	{Name: "GLM-5.3", ID: "cline-pass/glm-5.3"},
	{Name: "GLM-5.3 Flash", ID: "cline-pass/glm-5.3-flash"},
	{Name: "Kimi K3", ID: "cline-pass/kimi-k3"},
	{Name: "DeepSeek V4 Pro", ID: "cline-pass/deepseek-v4-pro"},
	{Name: "DeepSeek V4.1 Flash", ID: "cline-pass/deepseek-v4.1-flash"},
	{Name: "MiMo-V2.5", ID: "cline-pass/mimo-v2.5"},
	{Name: "MiMo-V2.5-Pro", ID: "cline-pass/mimo-v2.5-pro"},
	{Name: "MiniMax M3", ID: "cline-pass/minimax-m3"},
	{Name: "Muse Spark 1.3 Contributor", ID: "cline-pass/muse-spark-1.3-contributor"},
	{Name: "Qwen3.8 Max", ID: "cline-pass/qwen3.8-max"},
	{Name: "Qwen3.7 Max", ID: "cline-pass/qwen3.7-max"},
	{Name: "Qwen3.7 Plus", ID: "cline-pass/qwen3.7-plus"},
}

// ClinePassPresets returns a copy so callers cannot mutate the shared catalog.
func ClinePassPresets() []Preset {
	return append([]Preset(nil), clinePassPresets...)
}
