package identity

import "testing"

// The opt-in to the test lane is a label of the session, read from the environment on every call: it is on the
// roster so a sender can see who opted in, and taking the variable away leaves the lane at once.
func TestTheTestLaneOptInIsALabelThatFollowsTheEnvironment(t *testing.T) {
	fakeGit(t, "alpha", "main")
	t.Setenv("AIVOICEBUS_TEST", "")
	if l := labelsFor(t, ""); l["test"] != "" {
		t.Errorf("not opted in by default: %v", l)
	}
	t.Setenv("AIVOICEBUS_TEST", "1")
	if l := labelsFor(t, ""); l["test"] != "on" || !TestOptIn() {
		t.Errorf("AIVOICEBUS_TEST=1 opts in: %v", l)
	}
	for _, off := range []string{"0", "true", "on", "yes"} {
		t.Setenv("AIVOICEBUS_TEST", off) // exactly 1: a loose value is not an opt-in to receiving test traffic
		if l := labelsFor(t, ""); l["test"] != "" || TestOptIn() {
			t.Errorf("AIVOICEBUS_TEST=%s is not an opt-in: %v", off, l)
		}
	}
	// the labels a session start pinned (AIVOICEBUS_LABELS) do not keep a lane the environment left
	t.Setenv("AIVOICEBUS_TEST", "")
	if l := labelsFor(t, "repo=alpha,test=on"); l["test"] != "" || l["repo"] != "alpha" {
		t.Errorf("a pinned test=on is not an opt-in: %v", l)
	}
}
