package server

import (
	"github.com/baditaflorin/aivoicebus/internal/store"
	"net/http"
)

// Existing legacy Store views cannot return errors without breaking callers.
// Refuse their HTTP result if a checked raw read latched an integrity error,
// including routes that encode their own response rather than calling js.
type historyResponseWriter struct {
	http.ResponseWriter
	store   *store.Store
	started bool
	refused bool
}

func (w *historyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *historyResponseWriter) WriteHeader(status int) {
	if w.started {
		return
	}
	w.started = true
	if w.store.HealthChecked() != nil {
		w.refused = true
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Del("Content-Length")
		w.ResponseWriter.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.ResponseWriter.Write([]byte("store history is temporarily unavailable\n"))
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *historyResponseWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.WriteHeader(http.StatusOK)
	}
	if w.refused {
		return len(p), nil
	}
	return w.ResponseWriter.Write(p)
}
