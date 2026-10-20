package cli

import (
	"strings"
	"testing"
)

// The session-start hook shows a long broadcast as one line, but nothing else does: a person or an
// agent that asks for the message gets all of it.
func TestInboxAndThreadStillShowTheWholeOfALongBroadcast(t *testing.T) {
	mesh(t)
	body := "VM120 disk emergency over: disk grown 256 -> 512 GB\n" + strings.Repeat("removed node_modules of the idle run dirs, details follow. ", 25) + "\nLAST-LINE-OF-THE-BODY"
	t.Setenv("AIVOICEBUS_ME", "imac/claude.fce3")
	code, out, e := run(t, "", "send", "--to", "all", "--force", body)
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	id := strings.Fields(out)[1]

	t.Setenv("AIVOICEBUS_ME", "air/codex.a1b2")
	_, inbox, _ := run(t, "", "inbox")
	if !strings.Contains(inbox, "LAST-LINE-OF-THE-BODY") || strings.Contains(inbox, "for the rest") || strings.Contains(inbox, "…cut") {
		t.Errorf("inbox must show the whole body:\n%s", inbox)
	}
	_, thread, _ := run(t, "", "thread", id)
	if !strings.Contains(thread, "LAST-LINE-OF-THE-BODY") || strings.Contains(thread, "for the rest") || strings.Contains(thread, "…cut") {
		t.Errorf("thread must show the whole body:\n%s", thread)
	}
	_, js, _ := run(t, "", "inbox", "--json")
	if !strings.Contains(js, "LAST-LINE-OF-THE-BODY") {
		t.Errorf("inbox --json must carry the whole body:\n%s", js)
	}
}
