package connector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPanelLoadsTheConsoleThroughTheConnector(t *testing.T) {
	var lastAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!doctype html><html><head><title>UI</title></head><body>console</body></html>")
		case "/api/status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ready":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()

	service := NewService()
	if err := service.Configure(json.RawMessage(`{"proxy_url":"` + proxy.URL + `","management_token":"mgmt-token"}`)); err != nil {
		t.Fatal(err)
	}

	status, headers, body, err := service.HandleManagement(http.MethodGet, panelPrefix+"/panel", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("panel status = %d", status)
	}
	if contentType := strings.ToLower(headers.Get("Content-Type")); !strings.Contains(contentType, "text/html") {
		t.Fatalf("panel content type = %q", contentType)
	}
	page := string(body)
	if !strings.Contains(page, "<title>UI</title>") {
		t.Fatalf("panel did not serve the standalone console:\n%s", page)
	}
	// The console calls /api/... on its own origin; the shim must redirect those
	// calls to the connector's management routes.
	if !strings.Contains(page, panelPrefix) || !strings.Contains(page, "\"/api/\"") {
		t.Fatalf("panel is missing the API rewrite shim:\n%s", page)
	}

	status, _, body, err = service.HandleManagement(http.MethodGet, panelPrefix+"/status", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || !strings.Contains(string(body), "ready") {
		t.Fatalf("status call returned %d %s", status, body)
	}
	if lastAuth != "Bearer mgmt-token" {
		t.Fatalf("management token was not attached server-to-server: %q", lastAuth)
	}
}

func TestPanelExplainsAnUnreachableProxy(t *testing.T) {
	service := NewService()
	if err := service.Configure(json.RawMessage(`{"proxy_url":"http://127.0.0.1:1"}`)); err != nil {
		t.Fatal(err)
	}
	status, headers, body, err := service.HandleManagement(http.MethodGet, panelPrefix+"/panel", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK {
		t.Fatalf("panel status = %d", status)
	}
	if contentType := strings.ToLower(headers.Get("Content-Type")); !strings.Contains(contentType, "text/html") {
		t.Fatalf("panel content type = %q", contentType)
	}
	if !strings.Contains(string(body), "unavailable") {
		t.Fatalf("panel did not explain the failure:\n%s", body)
	}
}

func TestConnectorDeclaresNoInferenceCapability(t *testing.T) {
	service := NewService()
	for _, method := range []string{"executor.execute", "executor.execute_stream", "request.translate", "response.translate", "scheduler.pick"} {
		if _, err := service.Handle(method, nil); err == nil {
			t.Fatalf("%s must be rejected: no model traffic may cross the plugin RPC", method)
		}
	}
	payload := RegistrationPayload()
	if payload.Capabilities["management_api"] != true {
		t.Fatalf("management_api capability missing: %v", payload.Capabilities)
	}
	for _, capability := range []string{"executor", "scheduler", "interceptor", "translator"} {
		if _, present := payload.Capabilities[capability]; present {
			t.Fatalf("connector must not declare the %s capability", capability)
		}
	}
}
