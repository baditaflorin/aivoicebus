package msg

import (
	"strings"
	"testing"
)

func TestPersonOnlyIsTheAddressOrATagNotSentToOneSession(t *testing.T) {
	cases := []struct {
		to, body string
		want     bool
	}{
		{"air/human", "anything at all", true},
		{"AIR/Human", "anything", true},
		{"air", TokenHandTag + " air\nhands air its token", true},
		{"air", "  " + TokenHandTag + " air", true},
		{"air/claude", TokenHandTag + " air", true},        // every claude session there: not one
		{"air/*", PersonTag + " restart the router", true}, // whole machine
		{"all", ApprovalTag + " run it?", true},
		{"@repo=x", AskPrefix + "which one?", true},
		{"air/agent", TokenHandTag + " air", true},       // a terminal's default name, shared by any
		{"air/codex.k3", TokenHandTag + " air", false},   // the sender named one session: theirs
		{"air/florin", PersonTag + " look at it", false}, // a long-lived name is one session
		{"air", "build it\n" + TokenHandTag, false},      // only the first line counts
		{"air", "please see [for the person] below", false},
		{"air", "an ordinary task", false},
		{"*/human", "x", false}, // not a machine's person
		{"air/humans", "x", false},
	}
	for _, c := range cases {
		m := Message{ID: "a1b2c3d4e5", To: c.to, Body: c.body, Kind: KindTask}
		if got := PersonOnly(m); got != c.want {
			t.Errorf("PersonOnly(to %q, %q) = %v, want %v", c.to, c.body, got, c.want)
		}
	}
}

func TestPersonStepNamesWhatThePersonRuns(t *testing.T) {
	for body, want := range map[string]string{
		TokenHandTag + " air\n...": "aivoicebus token install a1b2c3d4e5",
		ApprovalTag + " run it?":   "aivoicebus approve a1b2c3d4e5",
		AskPrefix + "which one?":   "aivoicebus answer a1b2c3d4e5",
		PersonTag + " reboot it":   "aivoicebus claim a1b2c3d4e5",
	} {
		got := PersonStep(Message{ID: "a1b2c3d4e5", To: "air/human", Kind: KindTask, Body: body})
		if !strings.HasPrefix(got, want) {
			t.Errorf("%q: %q, want it to start %q", body, got, want)
		}
	}
}
