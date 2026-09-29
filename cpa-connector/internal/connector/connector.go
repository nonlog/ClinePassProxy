// Package connector implements the control-plane-only CPA plugin that exposes
// ClinePassProxy inside CPA/CPAMP.
//
// The connector registers a management API and a browser resource route. It
// deliberately implements no executor, scheduler, interceptor or translator:
// no inference traffic may pass through a CPA plugin executor RPC.
package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Version is the connector release version.
const Version = "0.1.0"

// PluginID is the stable plugin identifier.
const PluginID = "clinepassproxy"

// SchemaVersion matches the CPA plugin contract used by ClinePassBridge.
const SchemaVersion = 6

// DefaultProxyURL is the in-network address of the standalone service.
const DefaultProxyURL = "http://clinepassproxy:8788"

// Config is the connector's own configuration, supplied by the plugin host.
type Config struct {
	// ProxyURL is the ClinePassProxy base URL reachable from the CPA host.
	ProxyURL string `json:"proxy_url" yaml:"proxy_url"`
	// ManagementToken authenticates the connector to ClinePassProxy's
	// management API. It is never returned to a browser.
	ManagementToken string `json:"management_token" yaml:"management_token"`
	// TimeoutSeconds bounds a forwarded management call.
	TimeoutSeconds int `json:"timeout_seconds" yaml:"timeout_seconds"`
}

// DefaultConfig returns the connector defaults.
func DefaultConfig() Config {
	return Config{ProxyURL: DefaultProxyURL, TimeoutSeconds: 30}
}

// Service answers plugin host calls.
type Service struct {
	mu     sync.RWMutex
	config Config
	client *http.Client
}

// NewService builds a connector service.
func NewService() *Service {
	return &Service{
		config: DefaultConfig(),
		client: &http.Client{Timeout: 60 * time.Second},
	}
}

// Registration is the plugin registration payload announced to CPA.
type Registration struct {
	SchemaVersion int            `json:"schema_version"`
	Metadata      Metadata       `json:"metadata"`
	Capabilities  map[string]any `json:"capabilities"`
}

// Metadata mirrors the CPA plugin metadata block.
type Metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Description      string        `json:"Description"`
	ConfigFields     []ConfigField `json:"ConfigFields"`
}

// ConfigField mirrors the CPA plugin configuration field descriptor.
type ConfigField struct {
	Name        string   `json:"Name"`
	Type        string   `json:"Type"`
	EnumValues  []string `json:"EnumValues,omitempty"`
	Description string   `json:"Description"`
}

// Registration describes the connector to CPA.
//
// The capability set is intentionally limited to management integration.
// Adding executor, scheduler, interceptor or translator capabilities would put
// model traffic back on the plugin RPC boundary.
func RegistrationPayload() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             "ClinePassProxy Connector",
			Version:          Version,
			Author:           "nonlog",
			GitHubRepository: "https://github.com/nonlog/ClinePassProxy",
			Description:      "Control-plane entry for ClinePassProxy: management API and resource routes only, no model executor.",
			ConfigFields: []ConfigField{
				{Name: "proxy_url", Type: "string", Description: "ClinePassProxy base URL reachable from the CPA host, for example http://clinepassproxy:8788"},
				{Name: "management_token", Type: "string", Description: "ClinePassProxy management token used server-to-server by this connector"},
				{Name: "timeout_seconds", Type: "integer", Description: "Bound for a forwarded management call"},
			},
		},
		Capabilities: map[string]any{
			"management_api": true,
		},
	}
}

// Configure applies plugin-owned configuration.
//
// The host passes its own configuration document; the connector reads the
// `config_yaml` field when present and otherwise treats the payload as a plain
// settings object.
func (s *Service) Configure(raw json.RawMessage) error {
	next := DefaultConfig()
	if len(raw) > 0 {
		var envelope struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		_ = json.Unmarshal(raw, &envelope)
		trimmed := strings.TrimSpace(string(raw))
		if strings.HasPrefix(trimmed, "{") {
			var direct Config
			if err := json.Unmarshal(raw, &direct); err == nil {
				next = direct
			}
		}
		if len(envelope.ConfigYAML) > 0 {
			if err := decodeYAMLConfig(envelope.ConfigYAML, &next); err != nil {
				return err
			}
		}
	}
	next.ProxyURL = strings.TrimRight(strings.TrimSpace(next.ProxyURL), "/")
	if next.ProxyURL == "" {
		next.ProxyURL = DefaultProxyURL
	}
	parsed, err := url.Parse(next.ProxyURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return errors.New("proxy_url must be an absolute URL")
	}
	if next.TimeoutSeconds <= 0 {
		next.TimeoutSeconds = 30
	}
	if next.TimeoutSeconds > 300 {
		return errors.New("timeout_seconds must be between 1 and 300")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = next
	s.client = &http.Client{Timeout: time.Duration(next.TimeoutSeconds) * time.Second}
	return nil
}

// Config returns a copy of the active configuration, without the management
// token value.
func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.config
	out.ManagementToken = ""
	return out
}

// Routes lists the management API routes this connector exposes.
func (s *Service) Routes() []Route {
	patterns := []struct {
		method string
		path   string
		desc   string
	}{
		{"GET", "/v0/management/clinepassproxy/status", "ClinePassProxy health and summary"},
		{"GET", "/v0/management/clinepassproxy/config", "ClinePassProxy settings"},
		{"PUT", "/v0/management/clinepassproxy/config", "Update ClinePassProxy settings"},
		{"GET", "/v0/management/clinepassproxy/credentials", "Credential pool"},
		{"POST", "/v0/management/clinepassproxy/credentials", "Add a credential"},
		{"PUT", "/v0/management/clinepassproxy/credentials/{id}", "Update a credential"},
		{"DELETE", "/v0/management/clinepassproxy/credentials/{id}", "Delete a credential"},
		{"POST", "/v0/management/clinepassproxy/credentials/{id}/test", "Test a credential"},
		{"POST", "/v0/management/clinepassproxy/credentials/{id}/refresh", "Refresh credential quota"},
		{"GET", "/v0/management/clinepassproxy/models", "Model alias table"},
		{"POST", "/v0/management/clinepassproxy/models/test", "Issue a test completion"},
		{"GET", "/v0/management/clinepassproxy/usage", "Usage summary"},
		{"GET", "/v0/management/clinepassproxy/requests", "Request diagnostics"},
		{"GET", "/v0/management/clinepassproxy/requests/{id}", "One request diagnostic record"},
	}
	routes := make([]Route, 0, len(patterns)+1)
	for _, pattern := range patterns {
		routes = append(routes, Route{
			Method:      pattern.method,
			Path:        pattern.path,
			Description: pattern.desc,
			Options:     routeOptions(pattern.method, pattern.path),
		})
	}
	routes = append(routes, Route{
		Method:      "GET",
		Path:        "/v0/management/clinepassproxy/panel",
		Description: "Browser panel for ClinePassProxy",
		Menu:        "ClinePassProxy",
		Options:     routeOptions("GET", "/v0/management/clinepassproxy/panel"),
	})
	return routes
}

// Route is a management route advertised to CPA.
type Route struct {
	Method      string         `json:"method"`
	Path        string         `json:"path"`
	Description string         `json:"description,omitempty"`
	Menu        string         `json:"menu,omitempty"`
	Options     map[string]any `json:"options,omitempty"`
}

// HandleManagement forwards one management call to ClinePassProxy.
func (s *Service) HandleManagement(method, path string, query url.Values, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	s.mu.RLock()
	config := s.config
	client := s.client
	s.mu.RUnlock()

	if strings.HasSuffix(path, "/panel") {
		s.mu.RLock()
		base := s.config.ProxyURL
		s.mu.RUnlock()
		return http.StatusOK, http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}, []byte(panelHTML(base)), nil
	}

	target := upstreamPath(path)
	if target == "" {
		return http.StatusNotFound, nil, nil, fmt.Errorf("unsupported management path %s", path)
	}

	requestURL := config.ProxyURL + target
	if len(query) > 0 {
		requestURL += "?" + query.Encode()
	}
	request, err := http.NewRequest(method, requestURL, strings.NewReader(string(body)))
	if err != nil {
		return http.StatusBadGateway, nil, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/html")
	if token := strings.TrimSpace(config.ManagementToken); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for _, name := range []string{"Accept-Language"} {
		if value := headers.Get(name); value != "" {
			request.Header.Set(name, value)
		}
	}

	response, err := client.Do(request)
	if err != nil {
		return http.StatusBadGateway, nil, nil, fmt.Errorf("contact ClinePassProxy: %w", err)
	}
	defer response.Body.Close()
	// Management responses are small; the bound protects the plugin host.
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return http.StatusBadGateway, nil, nil, err
	}
	outHeaders := http.Header{}
	for _, name := range []string{"Content-Type"} {
		if value := response.Header.Get(name); value != "" {
			outHeaders.Set(name, value)
		}
	}
	return response.StatusCode, outHeaders, payload, nil
}

// upstreamPath maps a connector route onto the standalone management API.
func upstreamPath(path string) string {
	const prefix = "/v0/management/clinepassproxy"
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(path, prefix)
	switch {
	case rest == "/status":
		return "/api/status"
	case rest == "/config":
		return "/api/config"
	case rest == "/credentials" || strings.HasPrefix(rest, "/credentials/"):
		return "/api" + rest
	case rest == "/models" || strings.HasPrefix(rest, "/models/"):
		return "/api" + rest
	case rest == "/usage":
		return "/api/usage"
	case rest == "/requests" || strings.HasPrefix(rest, "/requests/"):
		return "/api" + rest
	default:
		return ""
	}
}

// Shutdown releases connector resources.
func (s *Service) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		s.client.CloseIdleConnections()
	}
}

// panelHTML is the browser entry shown inside CPAMP. It embeds the standalone
// UI so the two surfaces cannot drift apart.
func panelHTML(proxyURL ...string) string {
	base := ""
	if len(proxyURL) > 0 {
		base = strings.TrimRight(proxyURL[0], "/")
	}
	return strings.ReplaceAll(panelTemplate, "{{PROXY_BASE}}", base)
}

const panelTemplate = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ClinePassProxy</title>
<style>
  html, body { margin: 0; height: 100%; background: #0e1116; color: #e6edf3;
    font: 14px/1.5 ui-sans-serif, system-ui, "Segoe UI", sans-serif; }
  header { padding: 12px 16px; border-bottom: 1px solid #2a3140; display: flex; gap: 10px; align-items: center; }
  header b { font-weight: 600; }
  header span { color: #8b97a8; font-size: 12px; }
  iframe { border: 0; width: 100%; height: calc(100% - 45px); display: block; background: #0e1116; }
  a { color: #4c9aff; }
</style>
</head>
<body>
<header>
  <b>ClinePassProxy</b>
  <span id="hint">management console</span>
  <span style="margin-left:auto"><a id="external" href="#" target="_blank" rel="noreferrer">open standalone UI</a></span>
</header>
<iframe id="console" title="ClinePassProxy console"></iframe>
<script>
  const base = "{{PROXY_BASE}}";
  const target = base ? base + "/" : "";
  const link = document.getElementById("external");
  const hint = document.getElementById("hint");
  if (target) {
    document.getElementById("console").src = target;
    link.href = target;
  } else {
    hint.textContent = "set proxy_url in the connector configuration, then reload";
    link.style.display = "none";
  }
</script>
</body>
</html>`

func routeOptions(method, path string) map[string]any {
	return map[string]any{"method": method, "path": path}
}
