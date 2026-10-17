package tasks

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A node that says it holds reads open but answers every read at once must not be asked in a
// tight loop: an empty quick answer waits out the second.
func TestANodeThatAnswersAtOnceIsNotAskedInATightLoop(t *testing.T) {
	var reads atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health":
			json.NewEncoder(w).Encode(msg.Health{Node: "n", Version: "0.4.0", Features: []string{msg.FeatureInboxWait}})
		case "/v1/inbox":
			reads.Add(1)
			w.Header().Set(msg.HeaderGen, "7")
			w.Write([]byte("[]"))
		}
	}))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: "t"}
	start := time.Now()
	_, err := Wait(context.Background(), c, "a/b.1", Filter{Thread: "x"}, 3*time.Second, time.Hour)
	if err != ErrTimeout || time.Since(start) < 2*time.Second {
		t.Fatalf("wait: %v after %s", err, time.Since(start))
	}
	if n := reads.Load(); n > 5 {
		t.Errorf("%d reads in 3 seconds: a tight loop", n)
	}
}
