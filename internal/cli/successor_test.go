package cli

import (
	"strings"
	"testing"
	"time"
)

func TestSendOrSuccessorLetsTheNodesHandTheMessageToTheSessionThatReplacesAnEndedOne(t *testing.T) {
	s := deadSession(t)
	code, out, e := run(t, "", "send", "--to", "lap/claude.dead", "--or-successor", "the PR is ready: please look")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("not sent: %d %q %q", code, out, e)
	}
	for _, want := range []string{"its mail goes to the session that replaces it", "the newest live session in repo alpha on lap", "now lap/claude.live"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr is missing %q:\n%s", want, e)
		}
	}
	id, _ := sentID(t, out)
	if m := held(t, s, id); !m.ForSuccessor() || m.To != "lap/claude.dead" {
		t.Fatalf("the node holds %+v: marked for the successor, still addressed to the session that ended", m)
	}

	// the session that replaces it reads it, told whose it was
	session(t, "lap/claude.live", "repo=alpha")
	code, out, e = run(t, "", "inbox")
	if code != 0 || !strings.Contains(out, id) || !strings.Contains(out, "[inherited from claude.dead]") || !strings.Contains(out, "the PR is ready") {
		t.Fatalf("the successor's inbox: %d %q %q", code, out, e)
	}
	// ... and `status` tells the sender whom it got through to
	session(t, "desk/claude.me", "")
	if _, out, _ := run(t, "", "status", id); !strings.Contains(out, "in place of lap/claude.dead") && !strings.Contains(out, "not read yet by anyone") {
		t.Errorf("status:\n%s", out)
	}
	if _, out, _ := run(t, "", "inbox", "--json"); strings.Contains(out, "inherited") {
		t.Errorf("--json has a field the wire format does not: %q", out)
	}
}

func TestStatusSaysWhenAnotherSessionReadWhatWasWrittenToOne(t *testing.T) {
	s := deadSession(t)
	_, out, _ := run(t, "", "send", "--to", "lap/claude.dead", "--or-successor", "the PR is ready")
	id, _ := sentID(t, out)
	session(t, "lap/claude.live", "repo=alpha")
	run(t, "", "inbox", "--ack")
	session(t, "desk/claude.me", "")
	_, out, _ = run(t, "", "status", id)
	if !strings.Contains(out, "lap/claude.live") || !strings.Contains(out, "in place of lap/claude.dead") {
		t.Errorf("status does not say the read was in place of the session named:\n%s", out)
	}
	if n := s.Readers(id); n != 1 {
		t.Errorf("readers %d", n)
	}
}

func TestSendWithoutTheFlagIsKeptForTheSessionItNames(t *testing.T) {
	s := deadSession(t)
	_, out, _ := run(t, "", "send", "--to", "lap/claude.dead", "--queue", "keep this for it")
	id, _ := sentID(t, out)
	if held(t, s, id).ForSuccessor() {
		t.Error("a plain send carries the mark")
	}
	session(t, "lap/claude.live", "repo=alpha")
	if _, out, _ := run(t, "", "inbox"); strings.Contains(out, "keep this for it") {
		t.Errorf("an unmarked message was handed on:\n%s", out)
	}
	session(t, "desk/claude.me", "")
	_, out, _ = run(t, "", "send", "--to", "lap/claude.live", "now")
	if id, _ = sentID(t, out); held(t, s, id).ForSuccessor() {
		t.Error("a send to a live session carries the mark")
	}
}

func TestAReplyCarriesTheMarkUnlessItSaysNot(t *testing.T) {
	s := deadSession(t)
	session(t, "lap/claude.dead", "repo=alpha")
	s.SetClock(func() time.Time { return time.Now().Add(-3 * time.Hour) })
	_, out, _ := run(t, "", "send", "--to", "desk/claude.me", "--queue", "can you check this?")
	s.SetClock(time.Now)
	asked, _ := sentID(t, out)
	session(t, "desk/claude.me", "")
	_, out, e := run(t, "", "reply", "--re", asked, "checked, fine")
	id, _ := sentID(t, out)
	if !held(t, s, id).ForSuccessor() {
		t.Errorf("a reply is handed on by default: %q", e)
	}
	if !strings.Contains(e, "its mail goes to the session that replaces it") {
		t.Errorf("the reply says where it goes now:\n%s", e)
	}
	_, out, _ = run(t, "", "reply", "--re", asked, "--or-successor=false", "checked again, still fine")
	id, _ = sentID(t, out)
	if held(t, s, id).ForSuccessor() {
		t.Error("--or-successor=false keeps the reply for the session it names")
	}
	// a reply that cannot be handed on (to a machine) is a plain reply, not an error
	_, out, _ = run(t, "", "send", "--to", "lap", "--re", asked, "to the machine")
	if id, _ = sentID(t, out); held(t, s, id).ForSuccessor() {
		t.Error("a send to a machine carries the mark")
	}
	code, out, e := run(t, "", "reply", "--re", asked, "--to", "lap", "a reply to the machine")
	if id, _ = sentID(t, out); code != 0 || held(t, s, id).ForSuccessor() {
		t.Errorf("a reply that cannot be handed on goes as a plain one: %d %q %q", code, out, e)
	}
}

func TestOrSuccessorSaysWhyItCannotApply(t *testing.T) {
	s := deadSession(t)
	session(t, "desk/claude.me", "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--to", "lap", "x"}, "one exact session"},
		{[]string{"--to", "lap/claude", "x"}, "one exact session"},
		{[]string{"--to", "lap/codex.build1", "x"}, "a name that outlives a restart"},
		{[]string{"--to", "lap/claude.live", "--kind", "task", "x"}, "bookkeeping of one conversation"},
		{[]string{"--to", "lap/claude.live", "[for the person] run this"}, "for the person on a machine"},
		{[]string{"--to", "lap/claude.live", "--test", "x"}, "does not combine"},
		{[]string{"--to", "lap/claude.live", "--file", "/dev/null", "x"}, "does not combine"},
	} {
		code, out, e := run(t, "", append([]string{"send", "--or-successor"}, c.args...)...)
		if code != 2 || out != "" || !strings.Contains(e, c.want) || !strings.Contains(e, "--or-successor") {
			t.Errorf("%v: %d %q %q, want a usage error saying %q", c.args, code, out, e, c.want)
		}
	}
	if n := s.Stats().Messages; n != 0 {
		t.Errorf("%d message(s) were stored by refused sends", n)
	}
}

func TestOrSuccessorDoesNotHelpASessionWhoseRepoIsNotKnown(t *testing.T) {
	deadSession(t)
	code, _, e := run(t, "", "send", "--to", "lap/claude.9f9f", "--or-successor", "hello")
	if code == 0 || !strings.Contains(e, "refused: lap/claude.9f9f has not been seen") || !strings.Contains(e, "--or-successor does not help here") {
		t.Errorf("%d %q", code, e)
	}
}

func TestADryRunSaysTheMessageMayBeHandedOn(t *testing.T) {
	deadSession(t)
	_, out, _ := run(t, "", "send", "--to", "lap/claude.live", "--or-successor", "--dry-run", "x")
	if !strings.Contains(out, "successor") || !strings.Contains(out, "gone for 15m") {
		t.Errorf("dry run:\n%s", out)
	}
	if _, out, _ := run(t, "", "send", "--to", "lap/claude.live", "--dry-run", "x"); strings.Contains(out, "successor") {
		t.Errorf("a plain dry run mentions a successor:\n%s", out)
	}
}
