// Package translate converts between client protocols and Cline's Chat
// Completions upstream.
//
// The conversion must not disturb the stable prompt prefix: an extra field, a
// dropped whitespace-only block, or a reordered tool result changes the cache
// key upstream and destroys the warm prompt cache.
package translate

import (
	"encoding/json"

	"github.com/nonlog/ClinePassProxy/internal/config"
	"github.com/nonlog/ClinePassProxy/internal/models"
)

// NewID returns a short random hex identifier for generated protocol objects.
func NewID() string { return config.RandomTokenString() }

// JSONBytes marshals a value and panics only on programmer error.
func JSONBytes(value any) []byte {
	raw, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

// CloneMap returns a shallow copy of a JSON object.
func CloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// MergeObjects merges src into dst. When concat is true, string values are
// appended instead of replaced, which is what streaming deltas need.
func MergeObjects(dst, src map[string]any, concat bool) {
	for key, value := range src {
		if nested := models.Object(value); nested != nil {
			existing := models.Object(dst[key])
			if existing == nil {
				existing = map[string]any{}
				dst[key] = existing
			}
			MergeObjects(existing, nested, concat)
			continue
		}
		if concat {
			if text, ok := value.(string); ok {
				dst[key] = models.String(dst[key]) + text
				continue
			}
		}
		if value != nil {
			dst[key] = value
		}
	}
}

// CopyFields copies the listed keys when they are present in src.
func CopyFields(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

// TrimToLast keeps the trailing max bytes of a string.
func TrimToLast(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return value[len(value)-max:]
}

// StableJSON re-marshals a value with deterministic key order (Go maps marshal
// with sorted keys) so hashes and fixtures stay stable.
func StableJSON(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}
