package wants

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func cond(t *testing.T, s string) msg.Cond {
	t.Helper()
	c, err := msg.ParseCond(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReleaseCondition(t *testing.T) {
	e := Env{Release: "0.14.1"}
	for s, want := range map[string]bool{"release>=0.14.1": true, "release>=0.14.0": true, "release>=0.9.0": true, "release>=0.15.0": false, "release>=1.0.0": false} {
		if got := e.Met(cond(t, s)); got != want {
			t.Errorf("%s on 0.14.1: %v", s, got)
		}
	}
	if (Env{Release: "dev"}).Met(cond(t, "release>=0.1.0")) {
		t.Error("a build from source has no release: nothing is met")
	}
}

func TestHasConditionReadsTheFactsTheMachinePublishes(t *testing.T) {
	e := Env{Facts: func() msg.Facts {
		return msg.FactsOf(msg.Labels{msg.HasLabel: "signing-key+fleet-guard-0.3.13", msg.SSHLabel: "0own"})
	}}
	for s, want := range map[string]bool{"has:signing-key": true, "has:deploy-token": false, "has:fleet-guard:0.3.*": true, "has:fleet-guard:0.4.*": false, "has:ssh:0own": true, "has:ssh:other": false} {
		if got := e.Met(cond(t, s)); got != want {
			t.Errorf("%s: %v", s, got)
		}
	}
	unknown := Env{Facts: func() msg.Facts { return msg.Facts{} }}
	if unknown.Met(cond(t, "has:signing-key")) {
		t.Error("a machine that says nothing has no fact met: unknown is never yes")
	}
}

func TestFileConditionStaysUnderHomeAndNeverReadsAnything(t *testing.T) {
	home := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o755))
	must(os.WriteFile(filepath.Join(home, ".local", "bin", "agent-tail"), []byte("x"), 0o600))
	must(os.MkdirAll(filepath.Join(home, ".ssh"), 0o700))
	must(os.WriteFile(filepath.Join(home, ".ssh", "id_ed25519"), []byte("secret"), 0o600))
	outside := t.TempDir()
	must(os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600))
	must(os.Symlink(outside, filepath.Join(home, "link")))
	var opened []string
	e := Env{Home: home, Exists: func(p string) bool { opened = append(opened, p); _, err := os.Lstat(p); return err == nil }}
	for s, want := range map[string]bool{
		"file:~/.local/bin/agent-tail": true,
		"file:~/.local/bin/missing":    false,
		"file:~/.ssh/id_ed25519":       false, // a sensitive directory is never a condition, even though it exists
		"file:~/link/f":                false, // leaves the home directory by a symlink
	} {
		if got := e.Met(cond(t, s)); got != want {
			t.Errorf("%s: %v", s, got)
		}
	}
	for _, p := range opened {
		if rel, _ := filepath.Rel(home, p); rel == filepath.Join(".ssh", "id_ed25519") {
			t.Error("a sensitive path was even looked at")
		}
	}
	if (Env{Exists: func(string) bool { return true }}).Met(cond(t, "file:~/x")) {
		t.Error("no home directory, no file condition")
	}
}

func TestServiceConditionAsksTheProbeOnlyWithTheLabel(t *testing.T) {
	var asked []string
	e := Env{Loaded: func(l string) bool { asked = append(asked, l); return l == "com.badita.agent-tail" }}
	if !e.Met(cond(t, "service:com.badita.agent-tail")) || e.Met(cond(t, "service:nope")) {
		t.Error("service")
	}
	if len(asked) != 2 || asked[0] != "com.badita.agent-tail" {
		t.Errorf("%v", asked)
	}
}

// Split never settles what must be answered, and never reads a condition from anywhere but the trailer.
func TestSplitSettlesOnlyWhatMayBeSettled(t *testing.T) {
	e := Env{Release: "0.15.0"}
	met := msg.AppendWants("update it", msg.Wants{DoneWhen: "release>=0.15.0"})
	open := msg.AppendWants("update it", msg.Wants{DoneWhen: "release>=0.99.0"})
	ms := []msg.Message{
		{ID: "0000000001", From: "a/claude", To: "air", Body: met},
		{ID: "0000000002", From: "a/claude", To: "air", Body: open},
		{ID: "0000000003", From: "a/claude", To: "air/human", Body: msg.AppendWants(msg.ApprovalTag+" run it?", msg.Wants{DoneWhen: "release>=0.15.0"})},
		{ID: "0000000004", From: "a/claude", To: "air", Body: "a [done-when: release>=0.1.0] in the middle\nof the text"},
		{ID: "0000000005", From: "a/claude", To: "air", Body: "plain"},
	}
	left, done := Split(e, ms)
	if len(done) != 1 || done[0].Msg.ID != "0000000001" || done[0].Cond.String() != "release>=0.15.0" {
		t.Errorf("met: %+v", done)
	}
	if len(left) != 4 {
		t.Errorf("left: %+v", left)
	}
}
