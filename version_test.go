package main

import (
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
)

// The release is cut from VERSION, and the client tells people which nodes are "before 0.7.0" by
// comparing node releases with msg.RetractSince: a build whose own release is older than the one that
// introduced retractions would report itself as a node that does not apply them.
func TestVersionIsNotBeforeTheReleaseThatIntroducedRetractions(t *testing.T) {
	b, err := os.ReadFile("VERSION")
	if err != nil {
		t.Fatal(err)
	}
	v := strings.TrimSpace(string(b))
	if !release.ValidVersion(v) {
		t.Fatalf("VERSION is %q, not a release number", v)
	}
	if release.Newer(msg.RetractSince, v) {
		t.Errorf("VERSION is %s, before %s (msg.RetractSince), the release that introduced retractions: bump VERSION", v, msg.RetractSince)
	}
}
