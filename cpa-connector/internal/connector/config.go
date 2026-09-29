package connector

import (
	"encoding/json"
	"fmt"
	"strings"
)

// decodeYAMLConfig reads the plugin-owned YAML document. The connector only
// supports flat scalar keys, which keeps the connector free of a YAML dependency
// while remaining compatible with the host's `plugins.configs` layout.
func decodeYAMLConfig(document []byte, target *Config) error {
	if len(document) == 0 {
		return nil
	}
	trimmed := strings.TrimSpace(string(document))
	if strings.HasPrefix(trimmed, "{") {
		return json.Unmarshal([]byte(trimmed), target)
	}
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "proxy_url":
			target.ProxyURL = value
		case "management_token":
			target.ManagementToken = value
		case "timeout_seconds":
			var parsed int
			if _, err := fmt.Sscanf(value, "%d", &parsed); err != nil {
				return fmt.Errorf("timeout_seconds must be an integer")
			}
			target.TimeoutSeconds = parsed
		}
	}
	return nil
}
