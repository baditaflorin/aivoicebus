package server

import (
	"net/http"
	"os"
	"time"
)

// WithBootstrap serves one file (the client installer bundle) at
// /bootstrap/<key>.tgz so a new machine on the tailnet can fetch it. The path
// is an unguessable capability, the file must be fresh (mtime within maxAge),
// and everything else falls through to h. Turn off by removing the file.
func WithBootstrap(h http.Handler, key, file string, maxAge time.Duration) http.Handler {
	if key == "" || file == "" {
		return h
	}
	want := "/bootstrap/" + key + ".tgz"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != want {
			h.ServeHTTP(w, r)
			return
		}
		fi, err := os.Stat(file)
		if r.Method != http.MethodGet || err != nil || time.Since(fi.ModTime()) > maxAge {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		longWrite(w)
		http.ServeFile(w, r, file)
	})
}
