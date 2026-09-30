package serving

import (
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed web
var webAssets embed.FS

// uiAssets are inlined into index.html in this order. Keeping them as separate
// files makes the console reviewable in the repository; delivering them as one
// document is what lets the CPA connector fetch "/" once and serve the whole
// console from the CPA origin, with no browser access to the in-network proxy.
var uiAssets = []struct {
	Ext  string
	Path string
}{
	{".css", "web/theme.css"},
	{".css", "web/app.css"},
	{".js", "web/js/format.js"},
	{".js", "web/js/quota.js"},
	{".js", "web/js/theme.js"},
	{".js", "web/js/api.js"},
	{".js", "web/js/components.js"},
	{".js", "web/js/pages.js"},
	{".js", "web/js/app.js"},
}

const (
	uiCSSMarker = "<!--assets:css-->"
	uiJSMarker  = "<!--assets:js-->"
)

var (
	uiShellOnce sync.Once
	uiShell     []byte
	uiShellErr  error
)

// shell renders index.html with every asset inlined. It is built once: the
// embedded files cannot change while the process runs.
func shell() ([]byte, error) {
	uiShellOnce.Do(func() {
		raw, err := webAssets.ReadFile("web/index.html")
		if err != nil {
			uiShellErr = err
			return
		}
		page := string(raw)
		if !strings.Contains(page, uiCSSMarker) || !strings.Contains(page, uiJSMarker) {
			uiShellErr = fmt.Errorf("web/index.html is missing the asset markers")
			return
		}
		var css, js strings.Builder
		for _, asset := range uiAssets {
			content, err := webAssets.ReadFile(asset.Path)
			if err != nil {
				uiShellErr = fmt.Errorf("read %s: %w", asset.Path, err)
				return
			}
			if asset.Ext == ".css" {
				css.WriteString("\n/* " + asset.Path + " */\n")
				css.Write(content)
			} else {
				js.WriteString("\n// " + asset.Path + "\n")
				js.Write(content)
			}
		}
		page = strings.Replace(page, uiCSSMarker, "<style>"+css.String()+"</style>", 1)
		page = strings.Replace(page, uiJSMarker, "<script>"+js.String()+"</script>", 1)
		uiShell = []byte(page)
	})
	return uiShell, uiShellErr
}

// ui serves the embedded single-page management UI.
func (s *Server) ui() http.Handler {
	subtree, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(subtree))
	page, pageErr := shell()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveShell := func() {
			if pageErr != nil {
				http.Error(w, pageErr.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(page)
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" || path == "index.html" {
			serveShell()
			return
		}
		if _, err := fs.Stat(subtree, path); err != nil {
			// Single-page app: unknown paths fall back to the shell.
			serveShell()
			return
		}
		files.ServeHTTP(w, r)
	})
}
