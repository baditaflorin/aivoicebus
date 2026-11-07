//go:build unix

package server

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestRunFailedListenClosesIndexAndUnregistersSignals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRunFailedListenSignalCleanupChild$")
	cmd.Env = []string{"AIVOICEBUS_RUN_STOP_CHILD=1", "AIVOICEBUS_RUN_STOP_ROOT=" + t.TempDir()}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("child did not regain default termination signal behavior")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("failed startup retained signal handler or leaked writer: %v %s", err, output)
	}
}
func TestRunFailedListenSignalCleanupChild(t *testing.T) {
	if os.Getenv("AIVOICEBUS_RUN_STOP_CHILD") != "1" {
		t.Skip("owned subprocess only")
	}
	root := os.Getenv("AIVOICEBUS_RUN_STOP_ROOT")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(root, "synthetic-token")
	if err := os.WriteFile(token, []byte("synthetic-fixture-only-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	data := filepath.Join(root, "data")
	for i := 0; i < 4; i++ {
		err := Run(Options{Addr: held.Addr().String(), DataDir: data, Node: "synthetic", TokenFile: token, LeaseRetention: store.LeaseRetentionOptions{Mode: store.LeaseRetentionOn, Hosts: []string{"fixture-builder"}}})
		if err == nil {
			t.Fatal("occupied listen address accepted")
		}
	}
	// Only this fixture subprocess is signalled. A stale signal.Notify channel
	// would consume SIGTERM and the child would reach the failure below.
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	t.Fatal("failed Run retained a signal registration")
}
func TestDrainOnSignalCancelledStartDoesNotFlushClosedStore(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go drainOnSignal(ctx, make(chan os.Signal), &http.Server{}, st, done)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled startup drain goroutine leaked")
	}
	if _, err := os.Stat(filepath.Join(dir, "presence.json")); !os.IsNotExist(err) {
		t.Fatal("cancelled startup flushed an already-closed store", err)
	}
}
