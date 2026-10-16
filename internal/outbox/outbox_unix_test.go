//go:build unix

package outbox

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO where a secret file belongs would make a blocking read of it hang every send; Prepare
// must go on without it. (AIVOICEBUS_TOKEN comes from the environment in this test, as in the
// hook and MCP cases, so the token file is not what the client reads first.)
func TestAFIFOInTheConfigDirectoryDoesNotHangASend(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", many(1, "repo=alpha"))
	dir := os.Getenv("HOME") + "/.config/aivoicebus"
	os.MkdirAll(dir+"/machine-tokens", 0o700)
	for _, p := range []string{dir + "/token", dir + "/observer-token", dir + "/machine-tokens/pipe"} {
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skipf("no FIFOs here: %v", err)
		}
	}
	const observer = "Qm9ycm93ZWRUaW1lQW5kU3BhY2UxMjM0NTY3ODkw"
	os.WriteFile(dir+"/token.next", []byte(observer+"\n"), 0o600) // a real secret next to the FIFOs

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		_, err := Prepare(c, Request{To: "a/b", Body: "hello"}, os.Stderr)
		done <- result{err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Errorf("an ordinary message still goes: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Prepare blocked on a FIFO in the config directory")
	}
	if _, err := Prepare(c, Request{To: "a/b", Body: "the next token is " + observer}, os.Stderr); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("the secrets that can be read are still checked: %v", err)
	}
}
