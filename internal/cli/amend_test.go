package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

var amendedTag = regexp.MustCompile(`\[amended \d\d:\d\dZ\]`)

func TestAmendEndToEnd(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "deploy to prod at noon")

	t.Setenv("AIVOICEBUS_ME", "air/codex.a1") // reads the original before it is amended
	if code, out, _ := run(t, "", "inbox", "--ack"); code != 0 || !strings.Contains(out, "deploy to prod at noon") {
		t.Fatalf("%d %q", code, out)
	}

	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "", "amend", id, "deploy to prod at 13:00, not noon")
	m := regexp.MustCompile(`(?m)^amended ` + id + ` as ([0-9a-f]{10}) thread ` + id + `$`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("%d %q %q", code, out, e)
	}
	aid := m[1]
	for _, want := range []string{"replacing message " + id + " to air/codex: \"deploy to prod at noon\"", "1 other session(s) had read " + id, "undo: `aivoicebus retract " + aid + "`"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex.a1")
	_, inbox, _ := run(t, "", "inbox")
	if !strings.Contains(inbox, "[amended") || !strings.Contains(inbox, "this replaces message "+id+", which you read") || !strings.Contains(inbox, "deploy to prod at 13:00, not noon") || strings.Contains(inbox, "at noon\n") {
		t.Errorf("a reader of the original gets the correction, marked:\n%s", inbox)
	}

	t.Setenv("AIVOICEBUS_ME", "air/codex.a2") // never saw the original
	_, hook, _ := run(t, "", "hook", "--tool", "codex")
	if !amendedTag.MatchString(hook) || !strings.Contains(hook, "deploy to prod at 13:00, not noon") || strings.Contains(hook, "at noon") || strings.Contains(hook, "this replaces") {
		t.Errorf("a session that starts now gets the new text only:\n%s", hook)
	}

	_, thread, _ := run(t, "", "thread", id)
	if !strings.Contains(thread, "Amendments (each is replaced by the next): "+id+" -> "+aid) || !strings.Contains(thread, "[amended: replaced by "+aid) || !strings.Contains(thread, "deploy to prod at noon") {
		t.Errorf("the thread shows the chain and keeps the original:\n%s", thread)
	}
	if st := s.Stats(); st.Messages != 2 {
		t.Errorf("two messages: the original and its replacement: %d", st.Messages)
	}
}

func TestAmendTakesTheTextFromStdinAndAnAbbreviatedId(t *testing.T) {
	meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "status: healthy")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "status: NOT healthy\nsecond line\n", "amend", id[:6])
	if code != 0 || !strings.Contains(out, "\namended "+id+" as ") || !strings.Contains(e, "resolved "+id[:6]+" -> "+id) {
		t.Fatalf("%d %q %q", code, out, e)
	}
	_, th, _ := run(t, "", "thread", id)
	if !strings.Contains(th, "status: NOT healthy\nsecond line") {
		t.Errorf("%s", th)
	}
	if code, _, e := run(t, "", "amend", id[:5], "x"); code != 2 || !strings.Contains(e, "too short") {
		t.Errorf("%d %q", code, e)
	}
}

func TestAmendJSON(t *testing.T) {
	meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "status: healthy")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	code, out, e := run(t, "", "amend", id, "status: down", "--json")
	var v struct {
		ID, Amended, Thread, Report string
		DryRun                      bool `json:"dry_run"`
	}
	if err := json.Unmarshal([]byte(out), &v); code != 0 || err != nil || v.Amended != id || len(v.ID) != 10 || v.Thread != id || v.DryRun || !strings.Contains(v.Report, "amended "+id) {
		t.Fatalf("%d %v %q %q", code, err, out, e)
	}
	code, out, _ = run(t, "", "amend", id, "status: still down", "--dry-run", "--json")
	if err := json.Unmarshal([]byte(out), &v); code != 0 || err != nil || !v.DryRun || !strings.Contains(v.Report, "dry run: nothing was sent") {
		t.Fatalf("%d %v %q", code, err, out)
	}
}

func TestAmendRefusalsHaveExitCodesAndSendNothing(t *testing.T) {
	s := meshStore(t)
	id := sendID(t, "imac/claude.1", "air/codex", "status: healthy")
	t.Setenv("AIVOICEBUS_ME", "imac/claude.1")
	for name, c := range map[string]struct {
		args []string
		code int
		want string
	}{
		"no argument":    {[]string{"amend"}, 2, "usage: amend ID"},
		"empty id":       {[]string{"amend", "", "x"}, 2, "the id is empty"},
		"not an id":      {[]string{"amend", "zzzzzzzzzz", "x"}, 2, "not a message id"},
		"not there":      {[]string{"amend", "0123456789", "x"}, 3, "was not found on the nodes"},
		"unknown flag":   {[]string{"amend", "--bogus", id, "x"}, 2, ""},
		"no replacement": {[]string{"amend", id, "--dry-run"}, 2, "needs the corrected text"},
	} {
		if code, _, e := run(t, "", c.args...); code != c.code || !strings.Contains(e, c.want) {
			t.Errorf("%s: %d %q", name, code, e)
		}
	}
	t.Setenv("AIVOICEBUS_ME", "air/codex.a1") // another machine
	if code, _, e := run(t, "", "amend", id, "I did not send this"); code != 1 || !strings.Contains(e, "only that machine can amend it") {
		t.Errorf("%d %q", code, e)
	}
	if st := s.Stats(); st.Messages != 1 {
		t.Errorf("nothing was sent: %d messages", st.Messages)
	}
}

func TestHelpListsAmend(t *testing.T) {
	_, out, _ := run(t, "", "help")
	if !strings.Contains(out, "amend") {
		t.Errorf("%s", out)
	}
}
