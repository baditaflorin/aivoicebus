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
		PersonTag + " reboot it":   "already done or not needed: aivoicebus done a1b2c3d4e5 | to do it: aivoicebus claim a1b2c3d4e5",
	} {
		got := PersonStep(Message{ID: "a1b2c3d4e5", To: "air/human", Kind: KindTask, Body: body})
		if !strings.HasPrefix(got, want) {
			t.Errorf("%q: %q, want it to start %q", body, got, want)
		}
	}
}

// The task `fleet update` sent (0.11.4 to 0.13.x) is known by its fixed text, to the person on the
// machine it names, and nothing else is taken for one.
func TestUpdateTaskReleaseKnowsOnlyTheFleetUpdateTask(t *testing.T) {
	fleet := PersonTag + " Update aivoicebus on air: the nodes run 0.13.1. In a terminal there: aivoicebus update (it checks the signature before installing), then aivoicebus version."
	cases := []struct {
		name string
		m    Message
		want string
	}{
		{"the fleet update task", Message{To: "air/human", Kind: KindTask, Body: fleet}, "0.13.1"},
		{"a host in other case", Message{To: "AIR/human", Kind: KindTask, Body: fleet}, "0.13.1"},
		{"another machine's", Message{To: "imac/human", Kind: KindTask, Body: fleet}, ""},
		{"to the sessions, not the person", Message{To: "air", Kind: KindTask, Body: fleet}, ""},
		{"not a task", Message{To: "air/human", Body: fleet}, ""},
		{"untagged", Message{To: "air/human", Kind: KindTask, Body: strings.TrimPrefix(fleet, PersonTag+" ")}, ""},
		{"another person task", Message{To: "air/human", Kind: KindTask, Body: PersonTag + " restart the router"}, ""},
		{"no release in it", Message{To: "air/human", Kind: KindTask, Body: PersonTag + " Update aivoicebus on air: the nodes run soon. Thanks."}, ""},
		{"a release at the very end", Message{To: "air/human", Kind: KindTask, Body: PersonTag + " Update aivoicebus on air: the nodes run 0.14.0."}, "0.14.0"},
	}
	for _, c := range cases {
		if got := UpdateTaskRelease(c.m); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
