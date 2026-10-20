package viewer

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The address grammar and the identity reading are plain JavaScript (ui/lib.js, no DOM), checked
// under Node with a table that includes hostile hashes. Node is not part of the Go toolchain: a
// developer without it skips this test, CI sets AIVOICEBUS_REQUIRE_NODE so it cannot pass silently.
func TestLibJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("AIVOICEBUS_REQUIRE_NODE") != "" {
			t.Fatal("node is required (AIVOICEBUS_REQUIRE_NODE is set) but not installed")
		}
		t.Skip("no node: ui/lib.js not checked")
	}
	out, err := exec.Command(node, "testdata/lib_check.js").CombinedOutput()
	if err != nil {
		t.Fatalf("lib_check.js: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "ok ") {
		t.Errorf("unexpected output: %s", out)
	}
}

// lib.js is a plain script the page loads before viewer.js: the page must name it, the handler must
// serve it under the same CSP, and viewer.js must read what lib.js publishes.
func TestPageLoadsLibBeforeViewer(t *testing.T) {
	html, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	lib := strings.Index(string(html), `src="lib.js"`)
	view := strings.Index(string(html), `src="viewer.js"`)
	if lib < 0 || view < 0 || lib > view {
		t.Errorf("index.html must load lib.js before viewer.js (%d, %d)", lib, view)
	}
	js, _ := os.ReadFile("ui/viewer.js")
	if !regexp.MustCompile(`globalThis\)\.AVLib`).Match(js) {
		t.Error("viewer.js does not read AVLib")
	}
}
