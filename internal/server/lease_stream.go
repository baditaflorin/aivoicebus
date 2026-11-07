package server

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/store"
)

// The array retains the legacy wire contract. Only EOF permits its closing
// bracket: a failed/cancelled read after headers aborts the transport, so even
// an old client cannot accept an empty or partial successful lease set.
func streamLeases(w http.ResponseWriter, r *http.Request, s *store.Store) {
	it, err := s.NewLeaseIterator(r.Context())
	if err != nil {
		http.Error(w, "lease history is temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	defer it.Close()
	controller := http.NewResponseController(w)
	deadline, ok := it.Context().Deadline()
	if !ok {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = controller.SetWriteDeadline(deadline)
	defer controller.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.WriteString(w, "["); err != nil {
		return
	}
	first := true
	for {
		page, err := it.Next()
		if err == io.EOF {
			_, _ = io.WriteString(w, "]\n")
			return
		}
		if err != nil {
			panic(http.ErrAbortHandler)
		}
		for _, lease := range page {
			if it.Context().Err() != nil {
				panic(http.ErrAbortHandler)
			}
			b, err := json.Marshal(lease)
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			if !first {
				if _, err := io.WriteString(w, ","); err != nil {
					return
				}
			}
			first = false
			if _, err := w.Write(b); err != nil {
				return
			}
		}
		if err := controller.Flush(); err != nil && err != http.ErrNotSupported {
			return
		}
	}
}
