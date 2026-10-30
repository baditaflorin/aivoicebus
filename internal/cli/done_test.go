package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// inboxIDs is what a session of the machine is shown (the ids of an unread inbox).
func inboxIDs(t *testing.T, me string) string {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", me)
	_, out, _ := run(t, "", "inbox", "--json")
	var ms []msg.Message
	if err := json.Unmarshal([]byte(out), &ms); err != nil {
		return out
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	return strings.Join(ids, " ")
}

// `done` is the person's: it refuses inside an agent's session and settles nothing.
func TestDoneIsRefusedToAnAgent(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", "lap", "--kind", "task", "install the collector")
	id, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", sa)
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1")
	code, _, e := run(t, "", "done", id, "it is done")
	if code == 0 || !strings.Contains(e, "for the user, in their own terminal") {
		t.Errorf("an agent's done: %d %q", code, e)
	}
	if !strings.Contains(inboxIDs(t, sb), id) {
		t.Error("a refused done hid the task")
	}
}

// The person says it is done: no session on the machine is shown it, a session that starts later
// neither, the sender is told in the thread, the thread still reads it, the audit log says who.
func TestDoneHidesTheMessageFromEverySessionIncludingANewOne(t *testing.T) {
	mesh(t)
	present(t, sa)
	present(t, sb)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", "lap", "--kind", "task", "install the collector")
	task, _ := sentID(t, out)
	_, out, _ = run(t, "", "send", "--to", "lap", "a plain heads-up")
	plain, _ := sentID(t, out)
	if got := inboxIDs(t, "lap/claude.new1"); !strings.Contains(got, task) || !strings.Contains(got, plain) {
		t.Fatalf("a session of lap is shown both before: %q", got)
	}

	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "lap/agent")
	code, out, e := run(t, "", "done", task, plain, "did it by hand")
	if code != 0 || strings.Count(out, "settled ") != 2 {
		t.Fatalf("done: %d %q %q", code, out, e)
	}
	for _, me := range []string{sa, sb, "lap/claude.new2", "lap/codex.new3"} {
		if got := inboxIDs(t, me); strings.Contains(got, task) || strings.Contains(got, plain) {
			t.Errorf("%s is still shown a settled message: %q", me, got)
		}
	}
	var audited int
	es, _ := config.ReadAudit()
	for _, e := range es {
		if e.Action == "done" && e.Who == "lap/agent" && strings.Contains(e.Detail, "did it by hand") {
			audited++
		}
	}
	if audited != 2 {
		t.Errorf("the audit log says who settled what: %+v", es)
	}
	c, _ := client.FromEnv()
	ms, _ := c.Thread(task)
	if len(ms) != 2 || ms[1].From != "lap/human" || ms[1].Kind != msg.KindProgress || ms[1].Body != "[done by the person] did it by hand" || ms[1].To != boss {
		t.Errorf("the sender is told in the thread: %+v", ms)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	if code, th, _ := run(t, "", "thread", task); code != 0 || !strings.Contains(th, "install the collector") {
		t.Errorf("a settled message stays readable: %q", th)
	}
}

// A message that must be answered is never settled, whoever asks; done says what to run instead.
func TestDoneNeverSettlesWhatMustBeAnswered(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", "lap", "--kind", "task", msg.TokenHandTag+" lap\nhands lap its token")
	hand, _ := sentID(t, out)
	_, out, _ = run(t, "", "send", "--to", "lap/human", msg.AskPrefix+"which one?")
	ask, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", "lap/agent")
	t.Setenv("AIVOICEBUS_AGENT", "")
	code, _, e := run(t, "", "done", hand)
	if code == 0 || !strings.Contains(e, "must be answered") || !strings.Contains(e, "aivoicebus token install "+hand) {
		t.Errorf("a token hand: %d %q", code, e)
	}
	if code, _, e := run(t, "", "done", ask); code == 0 || !strings.Contains(e, "aivoicebus answer "+ask) {
		t.Errorf("a question: %d %q", code, e)
	}
	if got := inboxIDs(t, "lap/claude.new"); !strings.Contains(got, hand) {
		t.Errorf("the token hand is still shown: %q", got)
	}
}

// done --all settles what waits for the person, and leaves what needs an answer.
func TestDoneAllSettlesWhatWaitsForThePerson(t *testing.T) {
	mesh(t)
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	var tasks []string
	for _, body := range []string{"restart the router", "clean the disk"} {
		_, out, _ := run(t, "", "task", "lap", "--person", body)
		id, _ := sentID(t, out)
		tasks = append(tasks, id)
	}
	_, out, _ := run(t, "", "send", "--to", "lap/human", "--kind", "task", msg.TokenHandTag+" lap\nx")
	hand, _ := sentID(t, out)
	t.Setenv("AIVOICEBUS_ME", "lap/agent")
	t.Setenv("AIVOICEBUS_AGENT", "")
	code, out, e := run(t, "", "done", "--all", "handled")
	if code != 0 || strings.Count(out, "settled ") != 2 || !strings.Contains(e, "1 more must be answered") {
		t.Fatalf("done --all: %d %q %q", code, out, e)
	}
	c, _ := client.FromEnv()
	left, _ := c.Peek("lap/human")
	if len(left) != 1 || left[0].ID != hand {
		t.Errorf("only the token hand still waits: %+v", left)
	}
	if code, _, _ := run(t, "", "done"); code != exitUsage {
		t.Errorf("done needs ids or --all: %d", code)
	}
}

// Against nodes that predate settles a task is claimed and ended, and a plain message cannot be settled.
func TestDoneOnAnOldNodeFallsBackToClaimAndEnd(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	inner := server.NewHandler(s, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		var keep []string
		for _, f := range h.Features {
			if f != msg.FeatureSettle {
				keep = append(keep, f)
			}
		}
		h.Features = keep
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	}))
	defer srv.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_AGENT", "")
	present(t, sa)
	t.Setenv("AIVOICEBUS_ME", boss)
	_, out, _ := run(t, "", "send", "--to", "lap", "--kind", "task", "install the collector")
	task, _ := sentID(t, out)
	_, out, _ = run(t, "", "send", "--to", "lap", "a plain heads-up")
	plain, _ := sentID(t, out)

	t.Setenv("AIVOICEBUS_ME", "lap/agent")
	code, out, e := run(t, "", "done", plain, "x")
	if code == 0 || !strings.Contains(e, "older release that cannot settle a plain message") {
		t.Errorf("a plain message on an old node: %d %q %q", code, out, e)
	}
	code, out, e = run(t, "", "done", task, "did it")
	if code != 0 || !strings.Contains(out, "claimed and ended task "+task) {
		t.Fatalf("a task on an old node: %d %q %q", code, out, e)
	}
	c, _ := client.FromEnv()
	ms, _ := c.Thread(task)
	kinds := ""
	for _, m := range ms {
		kinds += m.Kind + ","
	}
	if kinds != "task,claim,end," {
		t.Errorf("claim, then end: %s", kinds)
	}
	if got := inboxIDs(t, sb); strings.Contains(got, task) {
		t.Errorf("a claimed task is not shown to the other sessions: %q", got)
	}
}
