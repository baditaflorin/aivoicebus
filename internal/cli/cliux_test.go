package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestExitCodeMapsEachKindOfFailureToItsOwnCode(t *testing.T) {
	status := func(code int) error { return &client.StatusError{Code: code, Status: fmt.Sprint(code)} }
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"success", nil, 0},
		{"reported already", exitError{7}, 7},
		{"plain failure", errors.New("boom"), exitFailure},
		{"no node configured", client.ErrNoURL, exitUsage},
		{"ambiguous id", fmt.Errorf("--re: %w", outbox.ErrAmbiguous), exitUsage},
		{"not found", outbox.NotFoundf("no such thing"), exitNotFound},
		{"usage", outbox.Usagef("bad"), exitUsage},
		{"wrapped usage", fmt.Errorf("--re: %w", outbox.Usagef("bad")), exitUsage},
		{"token refused", status(401), exitRejected},
		{"forbidden", fmt.Errorf("wrapped: %w", status(403)), exitRejected},
		{"other 4xx stays a failure", status(422), exitFailure},
	}
	for _, c := range cases {
		if got := exitCode(c.err); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	// The kinds that need a real error to build.
	_, err := (&client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "t"}).Who()
	if got := exitCode(err); got != exitUnreachable {
		t.Errorf("no node answered: %d (%v)", got, err)
	}
	if _, err := outbox.NewIDResolver(nil, "a/b").Resolve("zz"); exitCode(err) != exitUsage {
		t.Errorf("a malformed id is a usage error: %v", err)
	}
}

func TestEveryDataCommandHasJSONAndItParses(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "imac/claude")
	if code, out, e := run(t, "", "send", "--to", "air/codex", "hello"); code != 0 {
		t.Fatal(code, out, e)
	}
	t.Setenv("AIVOICEBUS_ME", "air/codex")

	var inbox []msg.Message
	runJSON(t, &inbox, "inbox")
	if len(inbox) != 1 || inbox[0].Body != "hello" {
		t.Errorf("inbox --json: %+v", inbox)
	}
	var thread []msg.Message
	runJSON(t, &thread, "thread", inbox[0].ID)
	if len(thread) != 1 || thread[0].ID != inbox[0].ID {
		t.Errorf("thread --json: %+v", thread)
	}
	var who struct {
		Sessions []msg.RosterEntry
		Hidden   int
	}
	runJSON(t, &who, "who")
	if len(who.Sessions) == 0 {
		t.Errorf("who --json: %+v", who)
	}
	var me struct{ Me, Why string }
	runJSON(t, &me, "whoami")
	if me.Me != "air/codex" {
		t.Errorf("whoami --json: %+v", me)
	}
	var ver struct{ Version, Release string }
	runJSON(t, &ver, "version")
	if ver.Version == "" {
		t.Errorf("version --json: %+v", ver)
	}
	var nodes struct {
		Nodes  []struct{ URL, Source string }
		Serial int64
	}
	runJSON(t, &nodes, "nodes")
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Source != "configured" {
		t.Errorf("nodes --json: %+v", nodes)
	}
	var q struct {
		Window string
		Active bool
	}
	runJSON(t, &q, "quiet")
	if q.Window != "off" || q.Active {
		t.Errorf("quiet --json: %+v", q)
	}
	var sent struct {
		ID, Thread, To string
		Reach          *int
	}
	runJSON(t, &sent, "send", "--to", "imac/claude", "again")
	if len(sent.ID) != 10 || sent.To != "imac/claude" {
		t.Errorf("send --json: %+v", sent)
	}
	var dry struct {
		DryRun   bool `json:"dry_run"`
		Priority string
	}
	runJSON(t, &dry, "send", "--dry-run", "--to", "imac/claude", "again")
	if !dry.DryRun || dry.Priority != "ask" {
		t.Errorf("send --dry-run --json: %+v", dry)
	}
	var checks []struct{ Name, Level string }
	runJSON(t, &checks, "doctor", "--offline")
	if len(checks) == 0 || checks[0].Name == "" {
		t.Errorf("doctor --json: %+v", checks)
	}
	var ret struct {
		ID     string
		DryRun bool `json:"dry_run"`
		Report string
	}
	runJSON(t, &ret, "retract", "--dry-run", sent.ID[:6])
	if ret.ID != sent.ID || !ret.DryRun || ret.Report == "" {
		t.Errorf("retract --json (by prefix): %+v", ret)
	}
}

// runJSON runs the command with --json and decodes stdout, which must be exactly one document.
func runJSON(t *testing.T, into any, args ...string) {
	t.Helper()
	code, out, e := run(t, "", append([]string{args[0], "--json"}, args[1:]...)...)
	if code != 0 && args[0] != "doctor" { // doctor exits 1 on a FAIL check, which is its job
		t.Fatalf("%v: code %d err %q", args, code, e)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(into); err != nil || dec.More() {
		t.Fatalf("%v: stdout is not one JSON document: %v\n%s", args, err, out)
	}
}

func TestInboxJSONIsAnEmptyArrayWhenThereIsNothing(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	code, out, _ := run(t, "", "inbox", "--json")
	if code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("%d %q", code, out)
	}
}

func TestExitCodesAndJSONFailuresAtTheCommandLine(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	// 3: nothing there.
	if code, out, e := run(t, "", "thread", "0123456789"); code != exitNotFound || out != "" || !strings.Contains(e, "no thread") {
		t.Errorf("thread of nothing: %d %q %q", code, out, e)
	}
	code, out, e := run(t, "", "retract", "0123456789", "--json")
	var f struct {
		Error string
		Exit  int
	}
	if code != exitNotFound || out != "" || json.Unmarshal([]byte(e), &f) != nil || f.Exit != exitNotFound || f.Error == "" {
		t.Errorf("--json failures are one JSON object on stderr, stdout empty: %d %q %q", code, out, e)
	}
	// 2: a malformed id, a missing id, a send with nothing to say: the command was used wrongly.
	if code, _, e := run(t, "", "ack", "zz"); code != exitUsage || !strings.Contains(e, "not a message id") {
		t.Errorf("malformed: %d %q", code, e)
	}
	// 4: the mesh is away.
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1")
	if code, _, e := run(t, "", "thread", "0123456789"); code != exitUnreachable {
		t.Errorf("unreachable: %d %q", code, e)
	}
	// 5: the token is refused.
	mesh(t)
	t.Setenv("AIVOICEBUS_TOKEN", "wrong")
	if code, _, e := run(t, "", "inbox"); code != exitRejected {
		t.Errorf("rejected: %d %q", code, e)
	}
}

func TestShortIDsWorkForReadingAndAmbiguityListsTheCandidates(t *testing.T) {
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	for _, id := range []string{"ab12000001", "ab12000002", "cd34000003"} {
		if _, err := s.Send(msg.Message{ID: id, From: "imac/claude", To: "air/codex", Body: "note " + id}); err != nil {
			t.Fatal(err)
		}
	}
	code, out, e := run(t, "", "thread", "ab12")
	if code != exitUsage || !strings.Contains(e, "ab12000001 (from imac/claude)") || !strings.Contains(e, "ab12000002") || strings.Contains(e, "cd34000003") {
		t.Fatalf("an ambiguous prefix names exactly its candidates and their senders: %d %q %q", code, out, e)
	}
	code, out, e = run(t, "", "thread", "cd34")
	if code != 0 || !strings.Contains(out, "note cd34000003") || !strings.Contains(e, "resolved cd34 -> cd34000003 from imac/claude") {
		t.Errorf("thread by prefix says what it resolved: %d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "", "thread", "cd34000003", "--json"); code != 0 || !strings.Contains(out, `"note cd34000003"`) {
		t.Errorf("--json after the id: %d %q", code, out)
	}
	// ack resolves all ids before it acknowledges any: an ambiguous one acks nothing.
	if code, _, _ := run(t, "", "ack", "cd34", "ab12"); code != exitUsage {
		t.Errorf("ack with an ambiguous id: %d", code)
	}
	if left := s.Inbox("air/codex", false); len(left) != 3 {
		t.Errorf("nothing was acknowledged: %d left", len(left))
	}
	if code, _, e := run(t, "", "ack", "cd34", "ab12000001"); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	if left := s.Inbox("air/codex", false); len(left) != 1 || left[0].ID != "ab12000002" {
		t.Errorf("%+v", left)
	}
	// send --re by prefix; --thread is never resolved by prefix.
	if code, out, e := run(t, "", "send", "--re", "ab120000", "ok"); code != exitUsage || !strings.Contains(e, "--re") {
		t.Errorf("an ambiguous --re: %d %q %q", code, out, e)
	}
	if code, out, e := run(t, "", "send", "--re", "cd340", "ok"); code != 0 || !strings.HasPrefix(out, "sent ") || !strings.Contains(e, "resolved cd340 -> cd34000003") {
		t.Errorf("send --re by prefix: %d %q %q", code, out, e)
	}
	code, out, _ = run(t, "", "send", "--to", "imac/claude", "--thread", "cd34", "--json", "x")
	var sent struct{ Thread string }
	if code != 0 || json.Unmarshal([]byte(out), &sent) != nil || sent.Thread != "cd34" {
		t.Errorf("--thread is taken as typed, not resolved: %d %q", code, out)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	for name, args := range map[string][]string{
		"a malformed id":     {"ack", "not-hex"},
		"a too short id":     {"thread", "ab"},
		"thread with no id":  {"thread"},
		"send with no body":  {"send", "--to", "imac/claude"},
		"a bad --within":     {"who", "--within", "soon"},
		"a bad quiet window": {"quiet", "tonight"},
		"a bad quiet for":    {"quiet", "for", "ever"},
		"retract two ids":    {"retract", "aaaaaaaaaa", "bbbbbbbbbb"},
	} {
		if code, _, e := run(t, "", args...); code != exitUsage {
			t.Errorf("%s: exit %d (%q), want 2", name, code, e)
		}
	}
	// Under --json a flag-parse error is the JSON error object too, and stdout stays empty.
	code, out, e := run(t, "", "send", "--bogus", "--json")
	var f struct {
		Error string
		Exit  int
	}
	if code != exitUsage || out != "" || json.Unmarshal([]byte(e), &f) != nil || f.Exit != exitUsage || !strings.Contains(f.Error, "bogus") {
		t.Errorf("flag error under --json: %d %q %q", code, out, e)
	}
	if code, out, e := run(t, "", "send", "--bogus"); code != exitUsage || out != "" || !strings.Contains(e, "flag provided but not defined") {
		t.Errorf("without --json the flag package speaks as before: %d %q %q", code, out, e)
	}
}

func TestQuietForIsBoundedAndTakesJSONInAnyPlace(t *testing.T) {
	mesh(t)
	for _, bad := range []string{"30s", "31d", "106752d", "9999999999999d", "0m", "-2h", "ever"} {
		if code, _, e := run(t, "", "quiet", "for", bad); code != exitUsage {
			t.Errorf("quiet for %s: %d %q", bad, code, e)
		}
	}
	if code, out, _ := run(t, "", "quiet"); !strings.Contains(out, "off") || code != 0 {
		t.Errorf("a refused setting changes nothing: %q", out)
	}
	var q struct {
		Active bool
		Until  *time.Time
	}
	code, out, e := run(t, "", "quiet", "for", "2h", "--json")
	if code != 0 || json.Unmarshal([]byte(out), &q) != nil || !q.Active || q.Until == nil || time.Until(*q.Until) < time.Hour+50*time.Minute {
		t.Fatalf("quiet for 2h --json: %d %q %q", code, out, e)
	}
	if code, out, _ := run(t, "", "quiet", "for", "30d"); code != 0 || !strings.Contains(out, "active now") {
		t.Errorf("30d is the limit and allowed: %d %q", code, out)
	}
	if code, _, _ := run(t, "", "quiet", "off"); code != 0 {
		t.Fatal(code)
	}
}

func TestSetupAndDoctorJSONFailureContracts(t *testing.T) {
	mesh(t)
	// setup that fails: stdout empty, the error object on stderr.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", file) // nothing can be created under a regular file
	code, out, e := run(t, "", "setup", "--yes", "--json")
	var f struct {
		Error string
		Exit  int
	}
	if code == 0 || out != "" || json.Unmarshal([]byte(e), &f) != nil || f.Exit != code {
		t.Errorf("setup --json failing: %d %q %q", code, out, e)
	}
	// doctor with a FAIL finding: exit 1 and the report on stdout (partial success), no error object.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", "")
	code, out, e = run(t, "", "doctor", "--json", "--offline")
	var checks []struct{ Name, Level string }
	if code != exitFailure || json.Unmarshal([]byte(out), &checks) != nil || len(checks) == 0 || strings.Contains(e, `"error"`) {
		t.Fatalf("doctor --json with a FAIL: %d %q %q", code, out, e)
	}
	failed := false
	for _, c := range checks {
		failed = failed || c.Level == "FAIL"
	}
	if !failed {
		t.Errorf("the report carries the FAIL that caused the exit: %+v", checks)
	}
}

func TestJSONOutputNeverCarriesTerminalControlsFromPeers(t *testing.T) {
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	evil := "ok\u009b31m \u202eevil\u2066 line\u2028two\u200b\x1b[2J"
	if _, err := s.Send(msg.Message{ID: "ee00000001", From: "imac/claude", To: "air/codex", Body: evil}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"inbox", "--json"}, {"thread", "ee00000001", "--json"}} {
		_, out, _ := run(t, "", args...)
		var ms []msg.Message
		if err := json.Unmarshal([]byte(out), &ms); err != nil || len(ms) != 1 {
			t.Fatalf("%v: %v %q", args, err, out)
		}
		if got := ms[0].Body; got != "ok evil line\ntwo" { // the strict rule removes the escape sequences whole, parameters included
			t.Errorf("%v: controls remain or the text was damaged: %q", args, got)
		}
	}
	s.InboxFor("peer/claude.1\u202e", false, msg.Labels{"repo": "al\u0085pha\u2066"})
	var who struct{ Sessions []msg.RosterEntry }
	runJSON(t, &who, "who")
	found := false
	for _, e := range who.Sessions {
		if strings.HasPrefix(e.Me, "peer/claude.1") {
			found = true
			if e.Me != "peer/claude.1" || e.Labels["repo"] != "alpha" {
				t.Errorf("who --json carries controls from a peer: %+v", e)
			}
		}
	}
	if !found {
		t.Errorf("the peer is not listed: %+v", who)
	}
	_, text, _ := run(t, "", "inbox")
	for _, bad := range []string{"\u009b", "\u202e", "\x1b", "\u200b"} {
		if strings.Contains(text, bad) {
			t.Errorf("the text rendering holds %q too", bad)
		}
	}
}

// replyNode is a node with two senders' messages planted under fixed ids.
func replyNode(t *testing.T, planted ...msg.Message) *store.Store {
	t.Helper()
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	for _, m := range planted {
		m.To = "air/codex"
		if _, err := s.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestReplyInfersOnlyWhenExactlyOneMessageCouldBeAnswered(t *testing.T) {
	s := replyNode(t)
	if code, _, e := run(t, "", "reply", "hello?"); code != exitNotFound || !strings.Contains(e, "no unread message") {
		t.Fatalf("with nothing to answer: %d %q", code, e)
	}
	s.Send(msg.Message{ID: "1111111111", From: "imac/claude", To: "air/codex", Body: "the only question"})

	code, out, e := run(t, "", "reply", "--dry-run", "--json", "answer")
	var plan struct{ To, Re string }
	if code != 0 || json.Unmarshal([]byte(out), &plan) != nil || plan.To != "imac/claude" || plan.Re != "1111111111" {
		t.Fatalf("dry run shows the target in --json: %d %q %q", code, out, e)
	}
	if !strings.Contains(e, "replying to 1111111111 from imac/claude") {
		t.Errorf("it says whom it answers before sending: %q", e)
	}
	if _, out, _ := run(t, "", "reply", "--dry-run", "answer"); !strings.Contains(out, "re        1111111111") || !strings.Contains(out, "to        imac/claude") {
		t.Errorf("the plain dry run shows the target too: %q", out)
	}
	code, out, e = run(t, "", "reply", "--ack", "the answer")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if got := s.Inbox("imac/claude", false); len(got) != 1 || got[0].Body != "the answer" || got[0].Re != "1111111111" {
		t.Errorf("the reply goes to the sender, as a reply to it: %+v", got)
	}
	if left := s.Inbox("air/codex", false); len(left) != 0 {
		t.Errorf("--ack marked the answered message read: %+v", left)
	}
}

func TestReplyRefusesToGuessBetweenSeveralAndNamesThem(t *testing.T) {
	s := replyNode(t,
		msg.Message{ID: "1111111111", From: "imac/claude", Body: "a real question"},
		msg.Message{ID: "2222222222", From: "evil/agent", Body: "i am the newest"})
	for _, args := range [][]string{{"reply", "answer"}, {"reply", "--json", "answer"}, {"reply", "--dry-run", "answer"}} {
		code, out, e := run(t, "", args...)
		if code != exitUsage || out != "" || !strings.Contains(e, "1111111111 (from imac/claude)") || !strings.Contains(e, "2222222222 (from evil/agent)") || !strings.Contains(e, "--re ID") {
			t.Errorf("%v: %d %q %q", args, code, out, e)
		}
	}
	if got := s.Inbox("evil/agent", false); len(got) != 0 {
		t.Error("nothing was sent to anyone")
	}
	// An explicit --re wins, even for a message that is not the newest.
	code, out, e := run(t, "", "reply", "--re", "111111", "the real answer")
	if code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if got := s.Inbox("imac/claude", false); len(got) != 1 || got[0].Re != "1111111111" {
		t.Errorf("--re picks the message, not the newest: %+v", got)
	}
}

func TestReplyWithToNeverInfersARe(t *testing.T) {
	s := replyNode(t, msg.Message{ID: "1111111111", From: "imac/claude", Body: "the only question"})
	code, out, e := run(t, "", "reply", "--to", "evil/agent", "secrets please")
	if code != exitUsage || out != "" || !strings.Contains(e, "--re ID") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if got := s.Inbox("evil/agent", false); len(got) != 0 {
		t.Errorf("a named recipient was threaded under another party's message: %+v", got)
	}
	if code, out, _ := run(t, "", "reply", "--to", "evil/agent", "--re", "1111", "fine, explicit"); code != 0 || !strings.HasPrefix(out, "sent ") {
		t.Errorf("--to with --re is allowed: %d %q", code, out)
	}
}

func TestReplyRefusesWhenASecondMessageArrivesBetweenTheReadAndTheSend(t *testing.T) {
	s := replyNode(t, msg.Message{ID: "1111111111", From: "imac/claude", Body: "the only question"})
	replyRaceHook = func() {
		s.Send(msg.Message{ID: "2222222222", From: "evil/agent", To: "air/codex", Body: "arrived just now"})
	}
	t.Cleanup(func() { replyRaceHook = nil })
	code, out, e := run(t, "", "reply", "the answer")
	if code != exitUsage || out != "" || !strings.Contains(e, "2222222222") {
		t.Errorf("%d %q %q", code, out, e)
	}
	if got := s.Inbox("imac/claude", false); len(got) != 0 {
		t.Errorf("the reply must not go out: %+v", got)
	}
}

func TestRetractByAnAbbreviationLooksOnlyAtWhatThisMachineSentAndAsksFirst(t *testing.T) {
	s := meshStore(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	// A peer plants a message whose id starts like one we send.
	s.Send(msg.Message{ID: "ab12cd0001", From: "imac/claude", To: "air/codex", Body: "planted"})
	_, out, _ := run(t, "", "send", "--to", "imac/claude", "mine")
	id := strings.Fields(out)[1]

	if code, _, e := run(t, "", "retract", "ab12cd"); code != exitNotFound || !strings.Contains(e, "retract looks nowhere else") {
		t.Errorf("an id that only the inbox holds is not retractable by prefix: %d %q", code, e)
	}
	if code, _, e := run(t, "", "retract", id[:5]); code != exitUsage || !strings.Contains(e, "at least 6") {
		t.Errorf("five characters are too few: %d %q", code, e)
	}
	// No terminal and no --yes: the target is named and nothing is retracted.
	code, _, e := run(t, "", "retract", id[:6])
	if code != exitUsage || !strings.Contains(e, "retract target: "+id+" to imac/claude, sent ") || !strings.Contains(e, "--yes") {
		t.Errorf("%d %q", code, e)
	}
	if got := s.Inbox("imac/claude", false); len(got) != 1 {
		t.Errorf("still delivered: %+v", got)
	}
	if code, _, e := run(t, "", "retract", id[:6], "--dry-run"); code != 0 || !strings.Contains(e, "retract target: "+id) {
		t.Errorf("a dry run names the target and needs no confirmation: %d %q", code, e)
	}
	code, out, e = run(t, "", "retract", id[:6], "--yes")
	if code != 0 || !strings.Contains(e, "resolved "+id[:6]+" -> "+id+" from air/codex") || !strings.Contains(out, id) {
		t.Errorf("%d %q %q", code, out, e)
	}
	if got := s.Inbox("imac/claude", false); len(got) != 0 {
		t.Errorf("retracted: %+v", got)
	}
	// The full id is as before: no confirmation.
	_, out, _ = run(t, "", "send", "--to", "imac/claude", "another")
	id2 := strings.Fields(out)[1]
	if code, _, e := run(t, "", "retract", id2); code != 0 {
		t.Errorf("a full id needs no --yes: %d %q", code, e)
	}
}
