package translate

import "strings"

// Claude thinking budget thresholds. These mirror the mapping CPA v8.0.4 uses
// between Anthropic `thinking.budget_tokens` and OpenAI `reasoning_effort`, so a
// request converted here reaches Cline with the same reasoning level it had
// before the migration.
const (
	thresholdMinimal = 512
	thresholdLow     = 1024
	thresholdMedium  = 8192
	thresholdHigh    = 24576
)

// BudgetToLevel converts an Anthropic thinking budget into a reasoning level.
// It returns false for budgets below -1, which the protocol does not define.
func BudgetToLevel(budget int64) (string, bool) {
	switch {
	case budget < -1:
		return "", false
	case budget == -1:
		return "auto", true
	case budget == 0:
		return "none", true
	case budget <= thresholdMinimal:
		return "minimal", true
	case budget <= thresholdLow:
		return "low", true
	case budget <= thresholdMedium:
		return "medium", true
	case budget <= thresholdHigh:
		return "high", true
	default:
		return "xhigh", true
	}
}

// NormalizeEffort lowercases and trims a reasoning effort value.
func NormalizeEffort(effort string) string {
	return strings.ToLower(strings.TrimSpace(effort))
}
