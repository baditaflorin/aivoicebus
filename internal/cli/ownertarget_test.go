package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
)

// send --to owner:SUBJECT goes to the one live session that has the record's needs, and to nobody else
// on the machine; what it did is said before it goes.
func TestSendToAnOwnerReachesTheSessionThatHasTheNeeds(t *testing.T) {
	ownerMesh(t)
	session(t, "boss/claude.a", "")
	code, out, e := run(t, "", "send", "--to", "owner:"+ownerSubject, "who tunes the pool on vm701?")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	for _, want := range []string{"owner:0mcp:vm701 -> imac needs role:pve (florin)", "-> imac/claude.pve1: has role:pve"} {
		if !strings.Contains(e, want) {
			t.Errorf("stderr lacks %q:\n%s", want, e)
		}
	}
	session(t, "imac/claude.pve1", "role=pve,tool=claude")
	if _, out, _ = run(t, "", "inbox"); !strings.Contains(out, "who tunes the pool on vm701?") {
		t.Errorf("the pve session gets it:\n%s", out)
	}
	session(t, "imac/claude.b5c9", "tool=claude")
	if _, out, _ = run(t, "", "inbox"); strings.Contains(out, "who tunes the pool") {
		t.Errorf("the other session of that machine must not:\n%s", out)
	}
	// a dry run resolves and describes, and sends nothing
	session(t, "boss/claude.a", "")
	if code, out, e := run(t, "", "send", "--dry-run", "--to", "owner:"+ownerSubject, "x"); code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(e, "-> imac/claude.pve1") {
		t.Errorf("dry run: %d %q %q", code, out, e)
	}
}

// With nobody live that has the needs, a send or a task goes to the person on the machine, marked so
// no agent takes it, and says so; an ask exits 3 and sends nothing; --person asks for the person
// whoever is live.
func TestAMessageToAnOwnerNobodyIsLiveForGoesToThePerson(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "imac/claude.b5c9", "tool=claude") // live, but not the workstream
	run(t, "", "inbox")
	session(t, "imac/claude.fce3", "tool=claude")
	if code, _, e := run(t, "", "owner", "set", ownerSubject, "--role", "pve"); code != 0 {
		t.Fatalf("owner set: %d %s", code, e)
	}
	session(t, "boss/claude.a", "")
	c, _ := client.FromEnv()
	personTask := func(out string) msg.Message {
		t.Helper()
		id, _ := sentID(t, out)
		ms, err := c.Thread(id)
		if err != nil || len(ms) != 1 || ms[0].To != "imac/human" || ms[0].Kind != msg.KindTask || !strings.HasPrefix(ms[0].Body, msg.PersonTag+" ") || !msg.PersonOnly(ms[0]) {
			t.Fatalf("not a task for the person on imac: %v %+v", err, ms)
		}
		return ms[0]
	}
	code, out, e := run(t, "", "task", "owner:"+ownerSubject, "the pool is full")
	if code != 0 || !strings.Contains(e, "no live session on imac has role:pve") || !strings.Contains(e, "task for the person") {
		t.Fatalf("task: %d %q %q", code, out, e)
	}
	if m := personTask(out); !strings.HasSuffix(m.Body, "the pool is full") {
		t.Errorf("body %q", m.Body)
	}
	code, out, e = run(t, "", "send", "--to", "owner:"+ownerSubject, "fyi: failover tonight")
	if code != 0 || !strings.Contains(e, "task for the person") {
		t.Fatalf("send: %d %q %q", code, out, e)
	}
	personTask(out)
	if code, _, e := run(t, "", "send", "--to", "owner:"+ownerSubject, "--kind", "question", "which pool?"); code != exitUsage || !strings.Contains(e, "without --kind") {
		t.Errorf("a question cannot wait in a person's task: %d %q", code, e)
	}
	code, out, e = run(t, "", "ask", "owner:"+ownerSubject, "which pool?")
	if code != exitNotFound || !strings.Contains(e, "nothing was sent") || !strings.Contains(e, "task owner:0mcp:vm701") {
		t.Errorf("ask with nobody live: %d %q %q", code, out, e)
	}
	// someone with the role is live now; --person still goes to the person
	session(t, "imac/claude.pve1", "role=pve")
	run(t, "", "inbox")
	session(t, "boss/claude.a", "")
	code, out, e = run(t, "", "task", "owner:"+ownerSubject, "--person", "please restart vm701")
	if code != 0 || !strings.Contains(e, "you asked for the person on imac") {
		t.Fatalf("task --person: %d %q %q", code, out, e)
	}
	personTask(out)
	if code, out, e = run(t, "", "task", "owner:"+ownerSubject, "check the pool"); code != 0 || !strings.Contains(e, "-> imac/claude.pve1") {
		t.Errorf("with the role holder live, the task goes to it: %d %q %q", code, out, e)
	}
}

// An owner record nobody pinned never carries an urgent message or a file; a pin on its declarer
// unlocks both, and `owner untrust` takes it back.
func TestAnUnpinnedOwnerRecordIsNeverUsedForUrgentOrFiles(t *testing.T) {
	ownerMesh(t)
	session(t, "boss/claude.a", "")
	code, out, e := run(t, "", "send", "--to", "owner:"+ownerSubject, "--prio", "urgent", "stop the pool")
	if code == 0 || !strings.Contains(e, "never used for an urgent message") || !strings.Contains(e, "aivoicebus owner trust 'imac/claude.*'") {
		t.Fatalf("urgent through an unpinned record: %d %q %q", code, out, e)
	}
	if code, out, e = run(t, "", "ask", "--prio", "urgent", "owner:"+ownerSubject, "is the pool stuck?"); code == 0 || !strings.Contains(e, "never used for an urgent message") {
		t.Fatalf("an urgent ask through an unpinned record: %d %q %q", code, out, e)
	}
	if code, _, e = run(t, "", "owner", "trust", "imac/claude.*"); code != 0 {
		t.Fatalf("owner trust: %d %q", code, e)
	}
	if _, out, _ = run(t, "", "owner", "trust"); strings.TrimSpace(out) != "imac/claude.*" {
		t.Errorf("owner trust lists: %q", out)
	}
	if code, out, e = run(t, "", "send", "--to", "owner:"+ownerSubject, "--prio", "urgent", "stop the pool"); code != 0 || !strings.Contains(e, "signed, pinned") {
		t.Errorf("urgent through a pinned record: %d %q %q", code, out, e)
	}
	if code, _, e = run(t, "", "owner", "trust", "*/claude.*"); code != exitUsage {
		t.Errorf("a pin that names no host: %d %q", code, e)
	}
	if code, _, e = run(t, "", "owner", "untrust", "imac/claude.*"); code != 0 {
		t.Fatalf("untrust: %d %q", code, e)
	}
	if code, _, _ = run(t, "", "owner", "untrust", "imac/claude.*"); code != exitNotFound {
		t.Errorf("untrust twice: %d", code)
	}
	if code, _, e = run(t, "", "send", "--to", "owner:"+ownerSubject, "--prio", "urgent", "stop the pool"); code == 0 {
		t.Errorf("after untrust: %q", e)
	}
}

// A claim made for another machine is shown and not used, until the sender trusts its declarer; two
// machines claiming the same subject are an error that names both, never a guess.
func TestOwnerClaimsForOthersAndConflicts(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	present(t, "imac/claude.pve1")
	session(t, "reg/claude.r1", "")
	if code, out, e := run(t, "", "owner", "set", "service:gateway", "--machine", "imac"); code != 0 || !strings.Contains(out, "names another machine as owner") || !strings.Contains(out, "aivoicebus owner trust 'reg/claude.*'") {
		t.Fatalf("owner set for another machine: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	code, _, e := run(t, "", "owner", "get", "service:gateway")
	if code != exitNotFound || !strings.Contains(e, "not used: record") || !strings.Contains(e, "whom you do not trust") {
		t.Errorf("a claim for someone else: %d %q", code, e)
	}
	if code, _, e := run(t, "", "send", "--to", "owner:service:gateway", "x"); code != exitNotFound || !strings.Contains(e, "not used: record") || !strings.Contains(e, "whom you do not trust") {
		t.Errorf("a send says which record it did not use: %d %q", code, e)
	}
	run(t, "", "owner", "trust", "reg/claude.*")
	if code, out, _ := run(t, "", "owner", "get", "service:gateway"); code != 0 || !strings.Contains(out, "-> imac") || !strings.Contains(out, "signed, pinned") {
		t.Errorf("after trusting the declarer: %d %q", code, out)
	}
	run(t, "", "owner", "untrust", "reg/claude.*")

	// air says it owns the gateway itself; with reg's claim for imac untrusted again, that is the only usable one
	present(t, "air/claude.a1")
	if code, out, e := run(t, "", "owner", "set", "service:gateway"); code != 0 {
		t.Fatalf("air set: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	if code, out, _ := run(t, "", "owner", "get", "service:gateway"); code != 0 || !strings.Contains(out, "-> air") {
		t.Errorf("the untrusted third-party claim is ignored: %d %q", code, out)
	}
	// imac now claims it for itself too: two owners
	session(t, "imac/claude.pve1", "")
	if code, out, e := run(t, "", "owner", "set", "service:gateway"); code != 0 {
		t.Fatalf("imac set: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	code, _, e = run(t, "", "owner", "get", "service:gateway")
	if code == 0 || !strings.Contains(e, "two owners are recorded") || !strings.Contains(e, "imac") || !strings.Contains(e, "air") || !strings.Contains(e, "aivoicebus owner trust") {
		t.Errorf("a conflict names both: %d %q", code, e)
	}
	if code, _, e := run(t, "", "send", "--to", "owner:service:gateway", "x"); code == 0 || !strings.Contains(e, "two owners are recorded") {
		t.Errorf("a send into a conflict sends nothing: %d %q", code, e)
	}
}

// A record that names a machine nobody has been heard on is refused, not sent into the void: the person
// there would never read it. The refusal says when the record expires and how it goes away.
func TestAMessageToAnOwnerOnAMachineNobodyIsHeardOnIsRefused(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	session(t, "reg/claude.r1", "")
	if code, _, e := run(t, "", "owner", "set", "service:old", "--machine", "ghost"); code != 0 {
		t.Fatalf("owner set: %d %q", code, e)
	}
	session(t, "boss/claude.a", "")
	run(t, "", "owner", "trust", "reg/claude.*")
	for _, args := range [][]string{{"send", "--to", "owner:service:old", "x"}, {"task", "owner:service:old", "x"}, {"ask", "owner:service:old", "x"}} {
		code, out, e := run(t, "", args...)
		if code == 0 || !strings.Contains(e, "which has never been seen") || !strings.Contains(e, "aivoicebus owner rm service:old") {
			t.Errorf("%v: %d %q %q", args, code, out, e)
		}
	}
	// the person's task is for a machine that exists: --person is refused for a ghost too
	if code, _, e := run(t, "", "task", "owner:service:old", "--person", "x"); code == 0 || !strings.Contains(e, "never been seen") {
		t.Errorf("--person to a machine nobody was heard on: %d %q", code, e)
	}
}

// A file or a log link lives minutes, so it cannot wait in a person's task: with nobody live it is refused,
// and the message is left as it was.
func TestApplyOwnerRouteRefusesAFileForThePerson(t *testing.T) {
	r := tasks.OwnerRoute{Machine: "imac", To: "imac/human", Person: true, Why: "nobody there is live",
		Res: msg.OwnerResolution{Subject: "0mcp:vm701", Record: msg.OwnerRecord{Subject: "0mcp:vm701", Machine: "imac", By: "imac/claude.a", At: time.Now(), Until: time.Now().Add(time.Hour)}}}
	to, kind, body := "owner:0mcp:vm701", "", "the file"
	var out, errOut strings.Builder
	err := applyOwnerRoute(r, nil, &to, &kind, &body, true, Streams{Out: &out, Err: &errOut})
	if err == nil || !strings.Contains(err.Error(), "cannot wait in a task for the person") || kind != "" || body != "the file" {
		t.Errorf("a file for the person: %v, kind %q, body %q", err, kind, body)
	}
	kind = "question"
	if err = applyOwnerRoute(r, nil, &to, &kind, &body, false, Streams{Out: &out, Err: &errOut}); err == nil || !strings.Contains(err.Error(), "without --kind") {
		t.Errorf("a question for the person: %v", err)
	}
	kind = ""
	if err = applyOwnerRoute(r, nil, &to, &kind, &body, false, Streams{Out: &out, Err: &errOut}); err != nil || to != "imac/human" || kind != msg.KindTask || body != msg.PersonTag+" the file" {
		t.Errorf("a text for the person: %v to %q kind %q body %q", err, to, kind, body)
	}
}
