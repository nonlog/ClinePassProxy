package serving

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/nonlog/ClinePassProxy/internal/models"
)

// SessionIdentity derives the stable identifier used for credential affinity.
//
// Preference order matters: the prompt cache lives upstream, so the identity
// must stay the same across every turn of one conversation.
func SessionIdentity(header map[string][]string, body map[string]any, configuredHeader string) (string, string) {
	if configuredHeader != "" {
		if value := firstHeader(header, configuredHeader); value != "" {
			return value, "configured-header"
		}
	}
	for _, key := range []string{"prompt_cache_key", "conversation_id", "conversation", "session_id", "thread_id"} {
		if value := models.String(body[key]); value != "" {
			return value, key
		}
	}
	for _, name := range []string{
		"X-Session-Id", "X-Conversation-Id", "X-Thread-Id", "Session-Id", "Conversation-Id",
		"X-Client-Request-Id", "X-Affinity-Key", "X-New-Api-Request-Id",
	} {
		if value := firstHeader(header, name); value != "" {
			return value, name
		}
	}
	if metadata := models.Object(body["metadata"]); metadata != nil {
		for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
			if value := models.String(metadata[key]); value != "" {
				return value, "metadata." + key
			}
		}
	}
	if key := prefixFingerprint(body); key != "" {
		return key, "prompt-prefix"
	}
	return "", "none"
}

func firstHeader(header map[string][]string, name string) string {
	for key, values := range header {
		if !strings.EqualFold(key, name) || len(values) == 0 {
			continue
		}
		return strings.TrimSpace(values[0])
	}
	return ""
}

// prefixFingerprint hashes the stable opening of a conversation. It is a weaker
// identity than an explicit session field, so it is only used as a fallback.
func prefixFingerprint(body map[string]any) string {
	for _, key := range []string{"messages", "input"} {
		value, ok := body[key]
		if !ok {
			continue
		}
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case []any:
			encoded, err := json.Marshal(typed)
			if err != nil {
				continue
			}
			text = string(encoded)
		default:
			continue
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len(text) > 4096 {
			text = text[:4096]
		}
		sum := sha256.Sum256([]byte(text))
		return "prefix:" + hex.EncodeToString(sum[:8])
	}
	return ""
}
