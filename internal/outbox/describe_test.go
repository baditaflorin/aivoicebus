package outbox

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// A reply's target came from a peer, and the node lets C1 controls through in an identity (they are
// not C0): the dry-run report shows it through the one display rule.
func TestDescribeShowsAPeersIdentityWithoutRawControls(t *testing.T) {
	var b strings.Builder
	Plan{Msg: msg.Message{From: "a/b", To: "x\u0085\u009b/c‮ode", Re: "ab\u0085cd", Body: "x"}}.Describe(&b)
	for _, bad := range []string{"\u0085", "\u009b", "‮"} {
		if strings.Contains(b.String(), bad) {
			t.Errorf("%q survived: %q", bad, b.String())
		}
	}
	if !strings.Contains(b.String(), "to        x?") {
		t.Errorf("%q", b.String())
	}
}
