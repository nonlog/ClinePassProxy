package connector

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	// calls to the connector's management routes and reuse CPAMP's saved
	// management login. Otherwise CPA itself rejects the rewritten call before
	// the connector can attach its server-to-server ClinePassProxy token.
	for _, want := range []string{panelPrefix, "\"/api/\"", "cli-proxy-auth", "enc::v2::", "Authorization"} {
		if !strings.Contains(page, want) {
			t.Fatalf("panel is missing %q from the CPA auth/rewrite shim:\n%s", want, page)
		}
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

func TestConnectorRegistersSingleMenuEntry(t *testing.T) {
	service := NewService()
	menuCount := 0
	for _, route := range service.Routes() {
		if route.Menu == "ClinePassProxy" {
			menuCount++
		}
	}
	for _, resource := range service.resourcePayload() {
		if resource["Menu"] == "ClinePassProxy" {
			menuCount++
		}
	}
	if menuCount != 1 {
		t.Fatalf("expected exactly one ClinePassProxy menu entry, got %d", menuCount)
	}
}

// Every path the console calls must be reachable through the connector,
// otherwise the panel works standalone and breaks inside CPAMP.
func TestConnectorForwardsEveryConsolePath(t *testing.T) {
	cases := map[string]string{
		"/v0/management/clinepassproxy/version":                 "/api/version",
		"/v0/management/clinepassproxy/status":                  "/api/status",
		"/v0/management/clinepassproxy/dashboard":               "/api/dashboard",
		"/v0/management/clinepassproxy/official":                "/api/official",
		"/v0/management/clinepassproxy/config":                  "/api/config",
		"/v0/management/clinepassproxy/credentials":             "/api/credentials",
		"/v0/management/clinepassproxy/credentials/www":         "/api/credentials/www",
		"/v0/management/clinepassproxy/credentials/www/test":    "/api/credentials/www/test",
		"/v0/management/clinepassproxy/credentials/www/refresh": "/api/credentials/www/refresh",
		"/v0/management/clinepassproxy/models":                  "/api/models",
		"/v0/management/clinepassproxy/models/test":             "/api/models/test",
		"/v0/management/clinepassproxy/models/providers/probe":  "/api/models/providers/probe",
		"/v0/management/clinepassproxy/usage":                   "/api/usage",
		"/v0/management/clinepassproxy/requests":                "/api/requests",
		"/v0/management/clinepassproxy/requests/req-1":          "/api/requests/req-1",
	}
	for path, want := range cases {
		if got := upstreamPath(path); got != want {
			t.Errorf("upstreamPath(%q) = %q, want %q", path, got, want)
		}
	}
	if got := upstreamPath("/v0/management/clinepassproxy/nope"); got != "" {
		t.Errorf("unknown path mapped to %q", got)
	}
}

func TestCPAExactManagementRoutesForwardCredentialActions(t *testing.T) {
	const id = "cline-5794b5c46996"
	type received struct{ method, path, auth, body string }
	calls := make(chan received, 8)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, _ := io.ReadAll(r.Body)
		calls <- received{r.Method, r.URL.Path, r.Header.Get("Authorization"), string(payload)}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer proxy.Close()

	service := NewService()
	if err := service.Configure(json.RawMessage(`{"proxy_url":"` + proxy.URL + `","management_token":"mgmt-token"}`)); err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool)
	for _, route := range service.Routes() {
		declared[route.Method+" "+route.Path] = true
		if strings.ContainsAny(route.Path, "{}*:") {
			t.Fatalf("CPA does not support dynamic route syntax: %s", route.Path)
		}
	}
	cases := []struct {
		method, route, target string
	}{
		{http.MethodPut, panelPrefix + "/credentials/item", "/api/credentials/" + id},
		{http.MethodDelete, panelPrefix + "/credentials/item", "/api/credentials/" + id},
		{http.MethodPost, panelPrefix + "/credentials/test", "/api/credentials/" + id + "/test"},
		{http.MethodPost, panelPrefix + "/credentials/refresh", "/api/credentials/" + id + "/refresh"},
		{http.MethodGet, panelPrefix + "/requests/item", "/api/requests/" + id},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.route, func(t *testing.T) {
			if !declared[tc.method+" "+tc.route] {
				t.Fatalf("CPA cannot dispatch unregistered route %s %s", tc.method, tc.route)
			}
			query := url.Values{"id": {id}}
			status, _, body, err := service.HandleManagement(tc.method, tc.route, query, nil, []byte(`{"label":"new-label"}`))
			if err != nil || status != http.StatusOK {
				t.Fatalf("forwarding returned %d %s, %v", status, body, err)
			}
			got := <-calls
			if got.method != tc.method || got.path != tc.target {
				t.Fatalf("upstream request = %s %s, want %s %s", got.method, got.path, tc.method, tc.target)
			}
			if got.auth != "Bearer mgmt-token" || got.body != `{"label":"new-label"}` {
				t.Fatalf("upstream auth or body changed: auth=%q body=%q", got.auth, got.body)
			}
		})
	}
}

func TestCPAResourceIDsCannotEscapeProxyRoutes(t *testing.T) {
	service := NewService()
	for _, id := range []string{"", "../status", "bad/key", "bad?key", "bad%2Fkey", strings.Repeat("a", 129)} {
		status, _, _, err := service.HandleManagement(http.MethodPut, panelPrefix+"/credentials/item",
			url.Values{"id": {id}}, nil, nil)
		if err != nil || status != http.StatusBadRequest {
			t.Errorf("id %q returned (%d, %v), want 400", id, status, err)
		}
	}
	shim := panelShim()
	for _, needle := range []string{"function target(i)", "credentials/", "requests/item?id=", "encodeURIComponent", "i=target(i)"} {
		if !strings.Contains(shim, needle) {
			t.Errorf("panel shim lacks %q for CPA exact-route mapping", needle)
		}
	}
}

func TestConnectorAdvertisesProviderProbeRoute(t *testing.T) {
	for _, route := range NewService().Routes() {
		if route.Method == http.MethodPost && route.Path == "/v0/management/clinepassproxy/models/providers/probe" {
			return
		}
	}
	t.Fatal("provider probe route is not advertised by the connector")
}
