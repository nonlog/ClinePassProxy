package connector

import (
	"encoding/json"
	"net/http"
	"net/url"
)

// Handle answers one plugin host method.
//
// Only registration, configuration and management methods are implemented. Any
// executor/translator/interceptor method is rejected, because the connector must
// stay out of the inference data plane.
func (s *Service) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if method == "plugin.reconfigure" && len(raw) > 0 {
			if err := s.Configure(raw); err != nil {
				return nil, err
			}
		}
		return RegistrationPayload(), nil
	case "management.register":
		return map[string]any{
			"routes":    s.routePayload(),
			"resources": s.resourcePayload(),
		}, nil
	case "management.handle":
		return s.handleManagementCall(raw)
	case "plugin.shutdown":
		s.Shutdown()
		return map[string]any{}, nil
	case "executor.execute", "executor.execute_stream", "executor.http_request",
		"scheduler.pick", "request.intercept", "response.intercept", "stream.chunk",
		"request.translate", "response.translate":
		return nil, errUnsupported
	default:
		return nil, errUnsupportedMethod(method)
	}
}

type managementCall struct {
	Method  string      `json:"Method"`
	Path    string      `json:"Path"`
	Headers http.Header `json:"Headers"`
	Query   url.Values  `json:"Query"`
	Body    []byte      `json:"Body"`
}

type managementReply struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers"`
	Body       []byte      `json:"Body"`
}

func (s *Service) handleManagementCall(raw json.RawMessage) (any, error) {
	var call managementCall
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
	}
	status, headers, body, err := s.HandleManagement(call.Method, call.Path, call.Query, call.Headers, call.Body)
	if headers == nil {
		headers = http.Header{}
	}
	headers.Set("Cache-Control", "no-store")
	headers.Set("X-Content-Type-Options", "nosniff")
	if err != nil && status == 0 {
		status = http.StatusBadGateway
	}
	return managementReply{StatusCode: status, Headers: headers, Body: body}, nil
}

// routePayload renders the management routes for the plugin host.
func (s *Service) routePayload() []map[string]any {
	routes := s.Routes()
	out := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		entry := map[string]any{
			"Method": route.Method,
			"Path":   route.Path,
		}
		if route.Description != "" {
			entry["Description"] = route.Description
		}
		if route.Menu != "" {
			entry["Menu"] = route.Menu
		}
		out = append(out, entry)
	}
	return out
}

// resourcePayload renders the browser-navigable resources for the plugin host.
func (s *Service) resourcePayload() []map[string]string {
	return []map[string]string{{
		"Path":        "/panel",
		"Menu":        "ClinePassProxy",
		"Description": "ClinePassProxy credentials, models, requests and usage",
	}}
}

type unsupportedMethodError struct{ method string }

func (e unsupportedMethodError) Error() string { return "unsupported plugin method: " + e.method }

func errUnsupportedMethod(method string) error { return unsupportedMethodError{method} }

// errUnsupported rejects inference-plane methods outright.
var errUnsupported = unsupportedMethodError{method: "the connector declares no inference capability"}
