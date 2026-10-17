package worker

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
)

// A replaced binary stops the loop before the next pass, with ErrReplaced (so a service restarts it).
func TestLoopStopsWhenItsBinaryIsReplaced(t *testing.T) {
	calls := 0
	w := &Worker{C: &client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "x"}, Out: io.Discard, Replaced: func() bool { calls++; return true }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Loop(ctx, time.Second); !errors.Is(err, ErrReplaced) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
