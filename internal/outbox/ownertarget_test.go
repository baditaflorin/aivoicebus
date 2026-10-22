package outbox

import (
	"io"
	"strings"
	"testing"
)

// A message built on owner:... that was not resolved first (a way of sending that does not know the form) is
// refused with what to do, never made into a message to a machine called owner:...
func TestPrepareRefusesAnOwnerTargetNobodyResolved(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	for _, to := range []string{"owner:0mcp:vm701", "OWNER:service:gateway", " owner:x "} {
		_, err := Prepare(c, Request{To: to, Body: "who owns it?"}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "is resolved before a message is made") || !strings.Contains(err.Error(), "aivoicebus owner get") {
			t.Errorf("%q: %v", to, err)
		}
	}
	if _, err := Prepare(c, Request{To: reader1, Body: "an ordinary message"}, io.Discard); err != nil {
		t.Errorf("an ordinary target: %v", err)
	}
}
