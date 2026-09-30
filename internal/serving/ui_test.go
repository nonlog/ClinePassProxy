package serving

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console is delivered as one document so the CPA connector can fetch "/"
// once and serve it from the CPA origin. If an asset stops being inlined, the
// panel silently loses a page, so assert the shell itself.
func TestShellInlinesEveryAsset(t *testing.T) {
	page, err := shell()
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	html := string(page)
	if strings.Contains(html, "<!--assets:") {
		t.Fatalf("asset marker left unreplaced")
	}
	for _, marker := range []string{
		"root.CPPFormat", "root.CPPQuota", "root.CPPTheme", "root.CPPApi",
		"root.CPPUI", "root.CPPPages", "root.CPPApp",
		"--accent:", ".quota-grid", ".modal-backdrop",
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("shell is missing %q", marker)
		}
	}
	// Load order matters: each file reads the namespace the previous one built.
	// The injected source comments name the files, so they cannot be confused
	// with a cross-file reference.
	previous := -1
	for _, asset := range uiAssets {
		index := strings.Index(html, "// "+asset.Path)
		if asset.Ext == ".css" {
			index = strings.Index(html, "/* "+asset.Path+" */")
		}
		if index < 0 {
			t.Fatalf("%s is missing from the shell", asset.Path)
		}
		if index < previous {
			t.Fatalf("%s is inlined out of order", asset.Path)
		}
		previous = index
	}
}

// The connector's shim rewrites same-origin "/api/..." fetches onto CPA. A
// console that called an absolute URL would bypass authentication, so the API
// client must talk to its own origin.
func TestShellAPICallsStaySameOrigin(t *testing.T) {
	page, err := shell()
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	html := string(page)
	if !strings.Contains(html, `fetch(path, init)`) {
		t.Errorf("api.js no longer fetches through the relative path helper")
	}
	if strings.Contains(html, "http://clinepassproxy") || strings.Contains(html, "https://clinepassproxy") {
		t.Errorf("shell must not embed the proxy's in-network address")
	}
}

func TestUIServesInlinedShell(t *testing.T) {
	server := &Server{}
	handler := server.ui()
	for _, path := range []string{"/", "/index.html", "/credentials"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s returned %d", path, recorder.Code)
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/html") {
			t.Fatalf("%s content type = %q", path, contentType)
		}
		if !strings.Contains(recorder.Body.String(), "root.CPPApp") {
			t.Fatalf("%s did not serve the inlined shell", path)
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/js/app.js", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/js/app.js returned %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "CPPApp") {
		t.Fatalf("/js/app.js served the wrong body")
	}
}
