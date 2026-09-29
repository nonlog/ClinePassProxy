package serving

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web
var webAssets embed.FS

// ui serves the embedded single-page management UI.
func (s *Server) ui() http.Handler {
	subtree, err := fs.Sub(webAssets, "web")
	if err != nil {
		return http.NotFoundHandler()
	}
	files := http.FileServer(http.FS(subtree))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if _, err := fs.Stat(subtree, path); err != nil {
			// Single-page app: unknown paths fall back to the shell.
			http.ServeFileFS(w, r, subtree, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}
