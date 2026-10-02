// Package models holds the model alias table and the shared JSON helpers used
// by every protocol package.
package models

import (
	"encoding/json"
	"strings"
)

// Entry maps a model ID that clients ask for onto the upstream Cline model ID.
type Entry struct {
	ID         string   `json:"id" yaml:"id"`
	UpstreamID string   `json:"upstream_id" yaml:"upstream_id"`
	Providers  []string `json:"providers,omitempty" yaml:"providers,omitempty"`
	Disabled   bool     `json:"disabled,omitempty" yaml:"disabled,omitempty"`
}

// Table resolves client-facing model IDs to upstream model IDs.
type Table struct {
	Entries []Entry
}

// Resolve returns the upstream model ID for a client-facing model ID.
// An empty alias table means the model ID is passed through unchanged.
func (t Table) Resolve(id string) (string, bool) {
	_, upstream, ok := t.ResolveEntry(id)
	return upstream, ok
}

// ResolveEntry returns the complete alias selected for a client-facing model
// ID, including any configured upstream provider allow-list.
func (t Table) ResolveEntry(id string) (Entry, string, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Entry{}, "", false
	}
	if len(t.Entries) == 0 {
		return Entry{ID: id, UpstreamID: id}, id, true
	}
	for _, entry := range t.Entries {
		if entry.ID == id {
			if entry.Disabled {
				return Entry{}, "", false
			}
			upstream := strings.TrimSpace(entry.UpstreamID)
			if upstream == "" {
				upstream = entry.ID
			}
			entry.Providers = append([]string{}, entry.Providers...)
			return entry, upstream, true
		}
	}
	return Entry{}, "", false
}

// DecodeObject unmarshals a JSON object, tolerating a JSON `null` element.
func DecodeObject(raw []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// Object returns the object stored at key, or nil when the value is absent or
// not an object.
func Object(value any) map[string]any {
	if value == nil {
		return nil
	}
	out, _ := value.(map[string]any)
	return out
}

// List returns the array stored at value, or nil when the value is absent or
// not an array.
func List(value any) []any {
	if value == nil {
		return nil
	}
	out, _ := value.([]any)
	return out
}

// String returns the string stored at value, or "" for any other type.
func String(value any) string {
	out, _ := value.(string)
	return out
}

// Number returns an integer view of a JSON number.
func Number(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

// Bool returns the boolean stored at value, or false for any other type.
func Bool(value any) bool {
	out, _ := value.(bool)
	return out
}

// Has reports whether the key is present with a non-nil value.
func Has(object map[string]any, key string) bool {
	if object == nil {
		return false
	}
	value, ok := object[key]
	return ok && value != nil
}
