package outbox

import (
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// meshWith starts a node, makes the process act as me, and registers sessions
// (identity -> labels) by having each read its inbox.
func meshWith(t *testing.T, me string, sessions map[string]string, opts ...server.Option) (*client.Client, *store.Store) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(server.NewHandler(s, "tok", opts...))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", me)
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Chdir(t.TempDir())
	for id, labels := range sessions {
		s.InboxFor(id, false, msg.ParseLabels(labels))
	}
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func many(n int, labels string) map[string]string {
	m := map[string]string{}
	for i := 0; i < n; i++ {
		m[fmt.Sprintf("host%d/claude.%04d", i, i)] = labels
	}
	return m
}

func TestAKindIsWarnedAboutWhenANodeWouldDropIt(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", many(1, "repo=alpha"), server.WithVersion("0.0.5"))
	var warn strings.Builder
	if _, err := Prepare(c, Request{To: "@repo=alpha", Body: "who owns this?", Kind: "question"}, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "drops the message kind") {
		t.Errorf("%q", warn.String())
	}
	warn.Reset()
	Prepare(c, Request{To: "@repo=alpha", Body: "plain note"}, &warn)
	if warn.Len() != 0 {
		t.Errorf("a plain message needs no warning: %q", warn.String())
	}
}

func TestDryRunReportsReachExpiryAndCostAndSendsNothing(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", many(3, "repo=alpha"))
	var warn strings.Builder
	p, err := Prepare(c, Request{To: "@repo=alpha", Body: "heads up: deploy window moved to 5pm", Prio: "ask"}, &warn)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	p.Describe(&out)
	for _, want := range []string{"dry run: nothing was sent", "to        @repo=alpha", "priority  ask", "expires   in 3d", "reaches   3 session(s)", "characters in each reader's session start"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
		}
	}
	if p.Msg.Reach == nil || *p.Msg.Reach != 3 {
		t.Errorf("the plan records how many it reaches: %+v", p.Msg.Reach)
	}
	if got := s.Thread(p.Msg.ID); len(got) != 0 || s.Stats().Messages != 0 {
		t.Error("preparing a message must not send it")
	}
	if p.Context <= len("heads up: deploy window moved to 5pm") {
		t.Errorf("the cost includes the header line: %d", p.Context)
	}
	if _, err := p.Send(c); err != nil || s.Stats().Messages != 1 {
		t.Errorf("sending the plan stores it: %v %d", err, s.Stats().Messages)
	}
}

func TestFYICostsNothingAtSessionStart(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", many(2, "repo=alpha"))
	p, err := Prepare(c, Request{To: "@repo=alpha", Body: "low value", Prio: "fyi"}, os.Stderr)
	if err != nil || p.Context != 0 {
		t.Fatalf("%v %d", err, p.Context)
	}
	var out strings.Builder
	p.Describe(&out)
	if !strings.Contains(out.String(), "fyi messages are only counted") || !strings.Contains(out.String(), "expires   in 1d") {
		t.Error(out.String())
	}
}

func TestGuardrailsStopAnUrgentStampedeAndABulkBroadcastUnlessForced(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", many(UrgentFanout, "repo=alpha"))
	_, err := Prepare(c, Request{To: "@repo=alpha", Body: "stop everything", Prio: "urgent"}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "would interrupt 5 sessions") {
		t.Fatalf("urgent fan-out: %v", err)
	}
	if _, err := Prepare(c, Request{To: "@repo=alpha", Body: "stop everything", Prio: "urgent", Force: true}, os.Stderr); err != nil {
		t.Errorf("--force overrides: %v", err)
	}
	if _, err := Prepare(c, Request{To: "@repo=alpha", Body: "stop everything", Prio: "ask"}, os.Stderr); err != nil {
		t.Errorf("only urgent is stopped: %v", err)
	}
	long := strings.Repeat("word ", BulkChars/5+10)
	if _, err := Prepare(c, Request{To: "@repo=alpha", Body: long}, os.Stderr); err == nil || !strings.Contains(err.Error(), "send it again shorter") {
		t.Errorf("a long body to many: %v", err)
	}
	if _, err := Prepare(c, Request{To: "me2/claude.9", Body: long}, os.Stderr); err != nil {
		t.Errorf("the same body to one session is fine: %v", err)
	}
}

func TestAReplyGoesBackToWhoeverWroteTheMessage(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", nil)
	q, _ := s.Send(msg.Message{From: "other/claude.2", To: "me/claude.0001", Body: "question?"})
	p, err := Prepare(c, Request{Re: q.ID, Body: "answer", Kind: "answer"}, os.Stderr)
	if err != nil || p.Msg.To != "other/claude.2" || p.Msg.Re != q.ID || p.Msg.Kind != msg.KindAnswer {
		t.Fatalf("%+v %v", p.Msg, err)
	}
	// replying to my own message continues to its recipient
	mine, _ := s.Send(msg.Message{From: "me/claude.0001", To: "other/claude.2", Body: "ping"})
	if p, err = Prepare(c, Request{Re: mine.ID, Body: "ping again"}, os.Stderr); err != nil || p.Msg.To != "other/claude.2" {
		t.Errorf("%+v %v", p.Msg, err)
	}
	if _, err := Prepare(c, Request{Re: "0000000000", Body: "x"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Errorf("%v", err)
	}
	if _, err := Prepare(c, Request{Body: "x", Kind: "answer", To: "a/b"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "--kind") {
		t.Errorf("an answer without --re is explained: %v", err)
	}
}

func TestTargetsAreExpandedBeforeTheyAreChecked(t *testing.T) {
	c, _ := meshWith(t, "mac-m1-mbp/claude.0001", map[string]string{
		"mac-m1-mbp/claude.0002": "machine=mac-m1-mbp,repo=alpha,tool=claude",
		"air/codex.0003":         "machine=air,repo=beta,tool=codex",
	})
	home := os.Getenv("HOME")
	os.MkdirAll(home+"/.config/aivoicebus", 0o755)
	os.WriteFile(home+"/.config/aivoicebus/groups", []byte("both=@repo=alpha|beta\n"), 0o644)
	t.Setenv("AIVOICEBUS_LABELS", "machine=mac-m1-mbp,tool=claude")

	p, err := Prepare(c, Request{To: "@+both", Body: "to the group"}, os.Stderr)
	if err != nil || p.Msg.To != "@repo=alpha|beta" || p.Reach != 2 {
		t.Errorf("a saved group: %q reach=%d %v", p.Msg.To, p.Reach, err)
	}
	p, err = Prepare(c, Request{To: "@here", Body: "same machine"}, os.Stderr)
	if err != nil || p.Msg.To != "@machine=mac-m1-mbp" || p.Reach != 1 {
		t.Errorf("here: %q reach=%d %v", p.Msg.To, p.Reach, err)
	}
	p, err = Prepare(c, Request{To: "@tool=claude|codex,repo!=beta", Body: "not beta"}, os.Stderr)
	if err != nil || p.Reach != 1 {
		t.Errorf("exclusion reach=%d %v", p.Reach, err)
	}
	if _, err := Prepare(c, Request{To: "@+nope", Body: "x"}, os.Stderr); err == nil {
		t.Error("an unknown group is an error, not a broadcast")
	}
	if _, err := Prepare(c, Request{To: "@repo=nobody", Body: "x"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "no session seen") {
		t.Errorf("%v", err)
	}
	if _, err := Prepare(c, Request{To: "@repo=nobody", Body: "x", Queue: true}, os.Stderr); err != nil {
		t.Errorf("--queue: %v", err)
	}
}

func TestCredentialsAreRefusedAndAnOlderNodeIsWarnedAbout(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", many(1, "repo=alpha"), server.WithVersion("0.0.5"))
	if _, err := Prepare(c, Request{To: "a/b", Body: "key ghp_" + strings.Repeat("a", 36)}, os.Stderr); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("%v", err)
	}
	var warn strings.Builder
	if _, err := Prepare(c, Request{To: "@repo=alpha|beta", Body: "hi"}, &warn); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "runs a release that cannot read alternatives") {
		t.Errorf("alternatives sent to a 0.0.5 node must say so: %q", warn.String())
	}
	warn.Reset()
	Prepare(c, Request{To: "@repo=alpha", Body: "hi"}, &warn)
	if strings.Contains(warn.String(), "runs a release that") {
		t.Errorf("plain selectors work everywhere: %q", warn.String())
	}
}

func TestASecretThisMachineHoldsIsRefusedWhateverItLooksLike(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", many(1, "repo=alpha"))
	dir := os.Getenv("HOME") + "/.config/aivoicebus"
	os.MkdirAll(dir+"/machine-tokens", 0o700)
	// 48 letters and digits is what deploy/ops.sh gen_token makes: no shape rule refuses it
	const observer = "Qm9ycm93ZWRUaW1lQW5kU3BhY2UxMjM0NTY3ODkw"
	const machine = "wUrKjC9xQrlSj7o0uzjugOIwbCxn6GMNLkApMfF9"
	os.WriteFile(dir+"/observer-token", []byte(observer+"\n"), 0o600)
	os.WriteFile(dir+"/machine-tokens/air", []byte(machine+"\n"), 0o600)
	t.Setenv("AIVOICEBUS_TOKEN", "a-token-from-the-environment-0123456789")

	for name, body := range map[string]string{
		"observer file": "for the dashboard use " + observer + " please",
		"machine file":  machine,
		"environment":   "my token: a-token-from-the-environment-0123456789",
	} {
		_, err := Prepare(c, Request{To: "a/b", Body: body}, os.Stderr)
		if err == nil || !strings.Contains(err.Error(), "refused: this message looks like it contains a secret from this machine's aivoicebus config") {
			t.Errorf("%s: %v", name, err)
		} else if strings.Contains(err.Error(), observer) || strings.Contains(err.Error(), machine) {
			t.Errorf("%s: the refusal repeats the secret: %v", name, err)
		}
	}
	// a reply goes through the same check
	q, _ := s.Send(msg.Message{From: "other/claude.2", To: "me/claude.0001", Body: "question?"})
	if _, err := Prepare(c, Request{Re: q.ID, Body: "the answer is " + observer, Kind: "answer"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a reply is checked too: %v", err)
	}
	var warn strings.Builder
	if _, err := Prepare(c, Request{To: "a/b", Body: "the tokens are in ~/.config/aivoicebus/observer-token and machine-tokens/"}, &warn); err != nil || warn.Len() != 0 {
		t.Errorf("naming the place is fine: %v %q", err, warn.String())
	}
}
