package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestAllowlistMatchesOnlyWhatItNames(t *testing.T) {
	a, bad := ParseAllow("# senders\ndesk\nlap/claude   # the laptop's Claude sessions\nLAP/codex.b2\n\n")
	if len(bad) != 0 || len(a.Entries()) != 3 {
		t.Fatalf("parse: %v %v", a.Entries(), bad)
	}
	for _, c := range []struct {
		from string
		want bool
	}{
		{"desk/claude.z9", true},   // any session on an allowed machine
		{"DESK/codex", true},       // names are compared in lower case
		{"desk", false},            // a bare machine name is not a sender
		{"deskx/claude.z9", false}, // a machine whose name starts with an allowed one
		{"evil/desk", false},
		{"lap/claude.a1", true},   // a session of an allowed tool
		{"lap/claude", true},      // the tool itself
		{"lap/claudex.a1", false}, // a tool whose name starts with the allowed one
		{"lap/codex.b2", true},    // the one allowed session
		{"lap/codex.b3", false},   // another session of that tool
		{"lap/worker", false},     // another session on that machine
		{"", false},
		{"desk/claude\n.z9", false}, // not a valid identity
	} {
		if _, got := a.Allows(c.from); got != c.want {
			t.Errorf("Allows(%q) = %v, want %v", c.from, got, c.want)
		}
	}
	if _, ok := (Allowlist{}).Allows("desk/claude.z9"); ok {
		t.Error("an empty allowlist allowed a sender")
	}
}

func TestAllowEntriesCannotMeanEveryone(t *testing.T) {
	for _, e := range []string{"", "all", "*", "desk/*", "*/claude", "@build", "aivoicebus", "a/b/c", "/claude", "desk/", "dësk", "lap claude", "Desk", "desk?"} {
		if CheckEntry(e) == nil {
			t.Errorf("CheckEntry(%q) accepted it", e)
		}
	}
	for _, e := range []string{"desk", "lap-b/claude", "lap_b/claude.a1b2", "10.0.0.7"} {
		if err := CheckEntry(e); err != nil {
			t.Errorf("CheckEntry(%q): %v", e, err)
		}
	}
	if a, bad := ParseAllow("all\n*\ndesk\n"); len(bad) != 2 || len(a.Entries()) != 1 {
		t.Errorf("bad lines: %v kept %v", bad, a.Entries())
	}
}

func TestTheAllowFileIsPrivateAndPlain(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if a, err := LoadAllow(); err != nil || !a.Empty() {
		t.Fatalf("no file: %v %v", a.Entries(), err)
	}
	if err := AddAllow("Desk"); err != nil {
		t.Fatal(err)
	}
	if err := AddAllow("desk"); err != nil { // twice is once
		t.Fatal(err)
	}
	if err := AddAllow("all"); err == nil {
		t.Error("AddAllow(all) was accepted")
	}
	p := AllowPath()
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("allow file mode: %v %v", fi, err)
	}
	if a, err := LoadAllow(); err != nil || strings.Join(a.Entries(), ",") != "desk" {
		t.Fatalf("after add: %v %v", a.Entries(), err)
	}
	os.Chmod(p, 0o644)
	if _, err := LoadAllow(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("a file others can read was trusted: %v", err)
	}
	os.Chmod(p, 0o600)
	os.WriteFile(p, []byte("desk\nall\n"), 0o600)
	if _, err := LoadAllow(); err == nil {
		t.Error("a file with a line that is not an entry was trusted")
	}
	os.Remove(p)
	other := filepath.Join(t.TempDir(), "list")
	os.WriteFile(other, []byte("desk\n"), 0o600)
	os.Symlink(other, p)
	if _, err := LoadAllow(); err == nil {
		t.Error("a symlinked allow file was followed")
	}
	os.Remove(p)
	AddAllow("desk")
	AddAllow("lap/claude")
	if found, err := RemoveAllow("desk"); !found || err != nil {
		t.Fatalf("rm: %v %v", found, err)
	}
	if a, _ := LoadAllow(); strings.Join(a.Entries(), ",") != "lap/claude" {
		t.Errorf("after rm: %v", a.Entries())
	}
	if found, _ := RemoveAllow("desk"); found {
		t.Error("removed an entry that was not there")
	}
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	a, _ := ParseAllow("desk\n")
	w := &Worker{Me: "lap/worker", Allow: a, MaxAge: time.Hour, Now: func() time.Time { return now }}
	task := func(from, to string, age time.Duration) msg.Message {
		return msg.Message{ID: "0123456789", Kind: msg.KindTask, From: from, To: to, At: now.Add(-age), Verified: msg.VerifiedSigned}
	}
	for _, c := range []struct {
		m    msg.Message
		take bool
		why  string
	}{
		{task("desk/claude.z9", "lap", time.Minute), true, ""},
		{task("desk/claude.z9", "lap/*", time.Minute), true, ""},
		{task("desk/claude.z9", "lap/worker", time.Minute), true, ""},
		{task("evil/claude.z9", "lap", time.Minute), false, "not in the allowlist"},
		{task("desk/claude.z9", "all", time.Minute), false, "not to this machine"},
		{task("desk/claude.z9", "@build", time.Minute), false, "not to this machine"},
		{task("desk/claude.z9", "lap/claude.a1", time.Minute), false, "not to this machine"},
		{task("desk/claude.z9", "lap", 2*time.Hour), false, "old"},
		{msg.Message{From: "desk/claude.z9", To: "lap", At: now}, false, "not a task"},
		{func() msg.Message {
			m := task("desk/claude.z9", "lap", time.Minute)
			m.Body = msg.TokenHandTag + " lap\nhands lap its token"
			return m
		}(), false, "the person there takes it (aivoicebus token install ID)"},
		{func() msg.Message {
			m := task("desk/claude.z9", "lap", time.Minute)
			m.Body = msg.PersonTag + " swap the disk in the hall"
			return m
		}(), false, "meant for the person on this machine (aivoicebus claim"},
		{func() msg.Message {
			m := task("desk/claude.z9", "lap", time.Minute)
			m.Verified = msg.VerifiedUnsigned
			return m
		}(), false, "not signed"},
		{func() msg.Message {
			m := task("desk/claude.z9", "lap", time.Minute)
			m.Verified = msg.VerifiedBad
			return m
		}(), false, "does not verify"},
		{func() msg.Message {
			m := task("desk/claude.z9", "lap", time.Minute)
			m.Verified = msg.VerifiedChanged
			return m
		}(), false, "KEY CHANGED"},
	} {
		why, _ := w.Decide(c.m)
		if (why == "") != c.take || !strings.Contains(why, c.why) {
			t.Errorf("Decide(from %s to %s, kind %q): %q", c.m.From, c.m.To, c.m.Kind, why)
		}
	}
	// --allow-unsigned admits a task with no signature, never a bad or changed one.
	w.AllowUnsigned = true
	for v, take := range map[string]bool{msg.VerifiedUnsigned: true, msg.VerifiedBad: false, msg.VerifiedChanged: false} {
		m := msg.Message{ID: "0123456789", Kind: msg.KindTask, From: "desk/claude.z9", To: "lap", At: now, Verified: v}
		if why, _ := w.Decide(m); (why == "") != take {
			t.Errorf("--allow-unsigned, %s: %q", v, why)
		}
	}
	w.AllowUnsigned = false
	w.Allow = Allowlist{}
	if why, _ := w.Decide(task("desk/claude.z9", "lap", time.Minute)); !strings.Contains(why, "allowlist is empty") {
		t.Errorf("empty allowlist: %q", why)
	}
}
