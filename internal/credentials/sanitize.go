package credentials

import (
	"regexp"
	"strings"
)

// secretPatterns catches Cline/OpenAI-style keys and bearer headers that could
// otherwise leak into a persisted health message or an error surfaced in the UI.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(sk|key|token|pk)[-_][A-Za-z0-9._-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._-]{8,}`),
	regexp.MustCompile(`(?i)\beyJ[A-Za-z0-9._-]{20,}`),
}

// sanitizeError bounds and redacts a message before it is stored or returned.
func sanitizeError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	for _, pattern := range secretPatterns {
		message = pattern.ReplaceAllString(message, "[REDACTED]")
	}
	if len(message) > 400 {
		message = message[:400]
	}
	return message
}

// Sanitize redacts secret-looking material from an arbitrary message.
func Sanitize(message string) string { return sanitizeError(message) }
