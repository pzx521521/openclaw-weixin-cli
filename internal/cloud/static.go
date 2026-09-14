package cloud

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// staticHandler serves web/dist with SPA fallback to index.html.
func (s *Server) staticHandler() http.Handler {
	fs := http.FileServer(http.Dir(s.web))
	index := filepath.Join(s.web, "index.html")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		clean := filepath.Clean("/" + r.URL.Path)
		p := filepath.Join(s.web, clean)
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			http.ServeFile(w, r, index)
			return
		}
		fs.ServeHTTP(w, r)
	})
}
