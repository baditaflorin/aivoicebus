package mcp

import (
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// aivoicebus_task and aivoicebus_ask with needs go to the session that has every capability, and
// send nothing when none has, listing the closest.
func TestTaskAndAskWithNeedsGoToTheSessionThatHasIt(t *testing.T) {
	s, st := start(t)
	goCommon := msg.Labels{"repo": "go-common", "origin": "baditaflorin/go-common", "tool": "claude"}
	web := msg.Labels{"repo": "web", "origin": "baditaflorin/web", "tool": "claude"}
	st.InboxFor("lap/claude.go1", false, goCommon)
	st.InboxFor("desk/claude.web1", false, web)

	text, isErr := s.call("aivoicebus_task", map[string]any{"needs": "repo:baditaflorin/go-common", "body": "bump the logger"})
	if isErr || !strings.HasPrefix(text, "needs: lap/claude.go1 (has repo:baditaflorin/go-common;") || !strings.Contains(text, "task ") {
		t.Fatalf("task with needs: %v %q", isErr, text)
	}
	if got := st.InboxFor("lap/claude.go1", false, goCommon); len(got) != 1 || got[0].To != "lap/claude.go1" || got[0].Body != "bump the logger" {
		t.Errorf("the go-common session's inbox: %+v", got)
	}
	if got := st.InboxFor("desk/claude.web1", false, web); len(got) != 0 {
		t.Errorf("the web session gets nothing: %+v", got)
	}

	text, isErr = s.call("aivoicebus_ask", map[string]any{"needs": "repo:web,gpu", "body": "which gpu?"})
	if isErr || !strings.HasPrefix(text, "NOBODY MATCHED") || !strings.Contains(text, "desk/claude.web1 (seen ") || !strings.Contains(text, "lacks gpu") {
		t.Fatalf("ask with needs nobody has: %v %q", isErr, text)
	}
	if text, isErr = s.call("aivoicebus_task", map[string]any{"body": "where?"}); !isErr || !strings.Contains(text, "or give needs") {
		t.Errorf("a task needs to or needs: %v %q", isErr, text)
	}
}
