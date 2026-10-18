package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// Throwaway tokens, built from pieces so no scanner takes them for real ones.
var (
	thShared = "shared" + "-" + strings.Repeat("s", 26)
	thSpare  = "spare" + "-" + strings.Repeat("p", 26)
	thAir    = "air" + "-" + strings.Repeat("m", 28)
	thImac   = "imac" + "-" + strings.Repeat("i", 27)
	thObs    = "observer" + "-" + strings.Repeat("o", 24)
)

// tokenTokens is the token file of the test nodes: the shared token, a spare unrestricted one,
// and the machine tokens of air and imac.
func tokenTokens(t *testing.T) *server.TokenSet {
	t.Helper()
	ts, err := server.ParseTokens("~spare:" + thSpare + "\nair:" + thAir + "\nimac:" + thImac + "\n")
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

type tokenNode struct {
	url string
	c   *client.Client // the shared token
	// refuseAirAfter, when > 0, makes the node refuse air's token after that many /v1/token
	// answers to it: a token revoked while it is being installed.
	refuseAirAfter atomic.Int32
	airAsked       atomic.Int32
}

// tokenMachine is the machine air with a node: its token file holds the shared token, nothing
// is approved or restarted for real (approveInstall and restartWorker answer as given).
func tokenMachine(t *testing.T, approve func() (string, error)) *tokenNode {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	n := &tokenNode{}
	h := server.NewHandler(s, thShared, server.WithNode("n1"), server.WithTokenSet(tokenTokens(t)), server.WithObserverToken(thObs))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+thAir && r.URL.Path == "/v1/token" {
			if k := n.airAsked.Add(1); n.refuseAirAfter.Load() > 0 && k > n.refuseAirAfter.Load() {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	n.url = srv.URL
	n.c = &client.Client{URLs: []string{srv.URL}, Token: thShared}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	t.Setenv("AIVOICEBUS_AGENT", "")
	t.Setenv("AIVOICEBUS_ME", "air/claude.1")
	t.Setenv("AIVOICEBUS_NOTIFY_URL", "")
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile(), []byte(thShared+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if approve != nil {
		old := approveInstall
		approveInstall = func(*client.Client, string, string, time.Duration, func(string, string), Streams) (string, error) {
			return approve()
		}
		t.Cleanup(func() { approveInstall = old })
	}
	oldRestart := restartWorker
	restartWorker = func() string { return "not installed" }
	t.Cleanup(func() { restartWorker = oldRestart })
	return n
}

func tokenFile() string { return filepath.Join(config.Dir(), "token") }

func approved() (string, error) { return "approver key", nil }

// handLink is the deploy side by hand: tok as a one-time file on the node, and the link message to
// air/claude.1 from deploy/claude.9. It returns the message id.
func (n *tokenNode) handLink(t *testing.T, tok string) string {
	t.Helper()
	l, _, err := (&attach.Node{URLs: []string{n.url}, Token: thShared}).Put(context.Background(), []byte(tok+"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	l.Name = "machine-token"
	out, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: "air/claude.1", Body: tokenLinkTag + " air\n" + l.Line()})
	if err != nil {
		t.Fatal(err)
	}
	return out.ID
}

func tokenNow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(tokenFile())
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// lastTo is the newest message in me's mail.
func (n *tokenNode) lastTo(t *testing.T, me string) msg.Message {
	t.Helper()
	ms, err := n.c.Inbox(me, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 {
		return msg.Message{}
	}
	return ms[len(ms)-1]
}

func TestTokenInstallPutsTheMachinesOwnTokenInPlaceAndAnswersWithIt(t *testing.T) {
	n := tokenMachine(t, approved)
	id := n.handLink(t, thAir)
	code, out, e := run(t, "", "token", "install", id, "--json")
	var res installResult
	if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil {
		t.Fatalf("install: %d %q %q (%v)", code, out, e, err)
	}
	if !res.Installed || res.Host != "air" || res.Kind != "machine" || res.Name != "air" || res.Previous != "shared" || res.Approved != "approver key" || res.Worker != "not installed" || res.From != "deploy/claude.9" || res.Answer == "" {
		t.Errorf("result: %+v", res)
	}
	if got := tokenNow(t); got != thAir {
		t.Error("the token file does not hold air's token")
	}
	if fi, _ := os.Stat(tokenFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	ans := n.lastTo(t, "deploy/claude.9")
	if ans.ID != res.Answer || !strings.HasPrefix(ans.Body, tokenInstalledTag+" air") || ans.Re != id {
		t.Errorf("the answer: %+v", ans)
	}
	// the node saw that answer sent with air's machine token: what `token hand` relies on
	seen, err := n.c.TokenSeen("air")
	if err != nil || len(seen) != 1 || seen[0].SendID != res.Answer || seen[0].SendKind != "machine" || seen[0].SendName != "air" {
		t.Errorf("seen: %+v %v", seen, err)
	}
	for _, s := range []string{out, e} {
		if strings.Contains(s, thAir) || strings.Contains(s, thShared) {
			t.Error("a token was printed")
		}
	}
	// the same again changes nothing and says so... but the link was read once and is gone
	if code, _, e := run(t, "", "token", "install", id); code == 0 || !strings.Contains(e, "expired or was read already") {
		t.Errorf("a second read of a one-time link: %d %q", code, e)
	}
	if tokenNow(t) != thAir {
		t.Error("a failed second install changed the file")
	}
}

func TestTokenInstallRefusesAnotherMachinesTokenOrAnUnboundOne(t *testing.T) {
	for _, tc := range []struct{ name, tok, want string }{
		{"another machine's", thImac, "the machine token of imac, and this machine is air"},
		{"an unbound one", thSpare, "a shared token, not a machine token"},
		{"the observer's", thObs, "the observer token"},
		{"one no node takes", "unknown" + "-" + strings.Repeat("u", 26), "the nodes do not accept it"},
		{"not a token", "two words " + strings.Repeat("w", 26), "not a single token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := tokenMachine(t, approved)
			id := n.handLink(t, tc.tok)
			code, out, e := run(t, "", "token", "install", id, "--json")
			var res installResult
			_ = json.Unmarshal([]byte(out), &res)
			if code == 0 || res.Installed || !strings.Contains(res.Error, tc.want) || !strings.Contains(res.Error, "nothing was changed") {
				t.Errorf("%d %+v %q", code, res, e)
			}
			if tokenNow(t) != thShared {
				t.Error("the token file was changed")
			}
			if ans := n.lastTo(t, "deploy/claude.9"); !strings.HasPrefix(ans.Body, tokenFailedTag+" air") || strings.Contains(ans.Body, tc.tok) {
				t.Errorf("the sender is told, without the token: %q", ans.Body)
			}
		})
	}
}

func TestTokenInstallOfAnExpiredLinkChangesNothing(t *testing.T) {
	n := tokenMachine(t, approved)
	id := n.handLink(t, thAir)
	ms, _ := n.c.Thread(id)
	if _, _, _, err := attach.FetchFile(context.Background(), ms[0].Body, thShared); err != nil { // someone else read it first
		t.Fatal(err)
	}
	if code, _, e := run(t, "", "token", "install", id); code == 0 || !strings.Contains(e, "expired or was read already") {
		t.Errorf("%d %q", code, e)
	}
	if tokenNow(t) != thShared {
		t.Error("the token file was changed")
	}
}

func TestTokenInstallPutsTheOldTokenBackWhenTheNewOneStopsWorking(t *testing.T) {
	n := tokenMachine(t, approved)
	n.refuseAirAfter.Store(2) // the checks before the switch pass; the one after it does not
	id := n.handLink(t, thAir)
	code, out, e := run(t, "", "token", "install", id, "--json")
	var res installResult
	_ = json.Unmarshal([]byte(out), &res)
	if code == 0 || res.Installed || !res.Restored || !strings.Contains(res.Error, "the old token is back in place") {
		t.Fatalf("%d %+v %q", code, res, e)
	}
	if tokenNow(t) != thShared {
		t.Error("the old token was not put back")
	}
	if fi, _ := os.Stat(tokenFile()); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if ans := n.lastTo(t, "deploy/claude.9"); !strings.HasPrefix(ans.Body, tokenFailedTag) {
		t.Errorf("the sender is told, with the old token: %q", ans.Body)
	}
}

func TestTokenInstallNeedsAPersonAndAMachineSignedSender(t *testing.T) {
	n := tokenMachine(t, nil) // the real approval: no terminal here, no approver key, no phone
	id := n.handLink(t, thAir)
	code, _, e := run(t, "", "token", "install", id)
	if code == 0 || !strings.Contains(e, "a person approves it") || !strings.Contains(e, "nothing was changed") {
		t.Errorf("without a terminal or an approval: %d %q", code, e)
	}
	if tokenNow(t) != thShared {
		t.Error("changed without an approval")
	}
	// nothing was fetched: the link is still good
	ms, _ := n.c.Thread(id)
	if _, _, _, err := attach.FetchFile(context.Background(), ms[0].Body, thShared); err != nil {
		t.Errorf("the refusal burned the link: %v", err)
	}

	// the token in the environment: the file is not what this machine uses
	t.Setenv("AIVOICEBUS_TOKEN", thShared)
	if code, _, e := run(t, "", "token", "install", id); code == 0 || !strings.Contains(e, "AIVOICEBUS_TOKEN is set") {
		t.Errorf("with AIVOICEBUS_TOKEN: %d %q", code, e)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "")

	// an unsigned link message is not taken
	n2 := tokenMachine(t, approved)
	l, _, _ := (&attach.Node{URLs: []string{n2.url}, Token: thShared}).Put(context.Background(), []byte(thAir), true)
	nc := &client.Client{URLs: n2.c.URLs, Token: thShared, NoSign: true}
	out, err := nc.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: "air/claude.1", Body: tokenLinkTag + " air\n" + l.Line()})
	if err != nil {
		t.Fatal(err)
	}
	if code, _, e := run(t, "", "token", "install", out.ID); code == 0 || !strings.Contains(e, "not signed") {
		t.Errorf("an unsigned sender: %d %q", code, e)
	}
	if tokenNow(t) != thShared {
		t.Error("changed for an unsigned sender")
	}
}

func TestTokenInstallWaitsForTheApproverKey(t *testing.T) {
	n := tokenMachine(t, nil)
	const pass = "a passphrase only the person knows"
	if _, err := config.CreateApprover(pass); err != nil {
		t.Fatal(err)
	}
	priv, err := config.OpenApprover(pass)
	if err != nil {
		t.Fatal(err)
	}
	id := n.handLink(t, thAir)
	done := make(chan [3]string, 1)
	go func() {
		var o, e strings.Builder
		code := Run([]string{"token", "install", id, "--wait", "1m"}, Streams{In: strings.NewReader(""), Out: &o, Err: &e})
		done <- [3]string{string(rune('0' + code)), o.String(), e.String()}
	}()
	var req msg.Message
	for i := 0; i < 200 && req.ID == ""; i++ {
		ms, _ := n.c.Inbox("air/human", false)
		for _, m := range ms {
			if strings.HasPrefix(m.Body, "[approval needed]") {
				req = m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req.ID == "" || !strings.Contains(req.Body, "aivoicebus approve") || strings.Contains(req.Body, thAir) {
		t.Fatalf("approval request: %+v", req)
	}
	if tokenNow(t) != thShared {
		t.Fatal("changed before the approval")
	}
	// a bare "approve", and one from another machine, are not the person's decision
	n.c.Send(msg.Message{ID: msg.NewID(), From: "air/claude.2", To: req.From, Re: req.ID, Thread: req.Thread, Body: "approve"})
	sig := ed25519.Sign(priv, msg.ApprovalStatement(req.ID, "approve", req.From))
	n.c.Send(msg.Message{ID: msg.NewID(), From: "imac/agent", To: req.From, Re: req.ID, Thread: req.Thread, Body: msg.ApprovalBody("approve", sig)})
	time.Sleep(500 * time.Millisecond)
	if tokenNow(t) != thShared {
		t.Fatal("a reply that is not the person's approval was taken as one")
	}
	n.c.Send(msg.Message{ID: msg.NewID(), From: "air/agent", To: req.From, Re: req.ID, Thread: req.Thread, Body: msg.ApprovalBody("approve", sig)})
	select {
	case r := <-done:
		if r[0] != "0" || !strings.Contains(r[1], "installed") || tokenNow(t) != thAir {
			t.Errorf("after the approval: %v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("install did not finish after the approval")
	}
}

// The task form, on one node: install takes the task, says ready, and waits for the link.
func TestTokenInstallTakesTheHandTaskAndSaysReadyBeforeTheLinkIsMade(t *testing.T) {
	n := tokenMachine(t, approved)
	task, err := n.c.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: "air", Kind: msg.KindTask, Prio: msg.PrioUrgent, Body: tokenHandTag + " air\nhands air its token"})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := n.c.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: "air", Kind: msg.KindTask, Body: tokenHandTag + " imac\nhands imac its token"})
	if code, _, e := run(t, "", "token", "install", other.ID); code == 0 || !strings.Contains(e, "hands imac its token, and this machine is air") {
		t.Errorf("a task for another machine: %d %q", code, e)
	}
	done := make(chan string, 1)
	go func() {
		var o, e strings.Builder
		Run([]string{"token", "install", task.ID, "--wait", "1m", "--json"}, Streams{In: strings.NewReader(""), Out: &o, Err: &e})
		done <- o.String()
	}()
	var ready msg.Message
	for i := 0; i < 300 && ready.ID == ""; i++ {
		ms, _ := n.c.Inbox("deploy/claude.9", false)
		for _, m := range ms {
			if strings.HasPrefix(m.Body, tokenReadyTag+" air") {
				ready = m
			}
			_ = n.c.Ack(m.ID, "deploy/claude.9")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ready.ID == "" || ready.From != "air/claude.1" || ready.Thread != task.Thread || !strings.Contains(ready.Body, "approved by the person (approver key)") {
		t.Fatalf("ready: %+v", ready)
	}
	l, _, _ := (&attach.Node{URLs: []string{n.url}, Token: thShared}).Put(context.Background(), []byte(thAir+"\n"), true)
	link, _ := n.c.Send(msg.Message{ID: msg.NewID(), From: "deploy/claude.9", To: ready.From, Re: ready.ID, Thread: task.Thread, Body: tokenLinkTag + " air\n" + l.Line()})
	var res installResult
	select {
	case out := <-done:
		if err := json.Unmarshal([]byte(out), &res); err != nil || !res.Installed || res.Task != task.ID {
			t.Fatalf("%q %+v", out, res)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("install did not finish")
	}
	ms, _ := n.c.Thread(task.ID)
	var claimed, answered bool
	for _, m := range ms {
		claimed = claimed || (m.Kind == msg.KindClaim && m.From == "air/claude.1")
		answered = answered || (m.ID == res.Answer && m.Re == link.ID && strings.HasPrefix(m.Body, tokenInstalledTag))
	}
	if !claimed || !answered {
		t.Errorf("the thread: claimed %v answered %v", claimed, answered)
	}
}

func TestTheTokenItselfIsNeverSentInAMessage(t *testing.T) {
	n := tokenMachine(t, approved)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude.9")
	dir := filepath.Join(config.Dir(), "machine-tokens")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "air"), []byte(thAir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	present(t, "air/claude.1")
	t.Setenv("AIVOICEBUS_ME", "deploy/claude.9")
	if code, _, e := run(t, "", "send", "--to", "air/claude.1", "here it is: "+thAir); code == 0 || !strings.Contains(e, "refused") {
		t.Errorf("a raw machine token in a message: %d %q", code, e)
	}
	// kept somewhere else (machine-token.sh's AIVOICEBUS_MACHINE_TOKENS_DIR): refused the same
	other := t.TempDir()
	os.Rename(filepath.Join(dir, "air"), filepath.Join(other, "air"))
	t.Setenv("AIVOICEBUS_MACHINE_TOKENS_DIR", other)
	if _, found := outbox.Credential("x " + thAir); !found {
		t.Error("a token in AIVOICEBUS_MACHINE_TOKENS_DIR is not one the send scan knows")
	}
	if code, _, e := run(t, "", "send", "--to", "air/claude.1", thAir); code == 0 || !strings.Contains(e, "refused") {
		t.Errorf("a raw machine token from the other directory: %d %q", code, e)
	}
	// the link message passes the same scan
	id := n.handLink(t, thAir)
	ms, _ := n.c.Thread(id)
	if kind, found := outbox.Credential(ms[0].Body); found {
		t.Errorf("the link message looks like a %s to the scan", kind)
	}
}

func TestTokenHandRefusesWhatItCannotHandOver(t *testing.T) {
	n := tokenMachine(t, approved)
	t.Setenv("AIVOICEBUS_ME", "deploy/claude.9")
	dir := filepath.Join(config.Dir(), "machine-tokens")
	os.MkdirAll(dir, 0o700)
	for _, tc := range []struct {
		args       []string
		file, want string
		mode       os.FileMode
	}{
		{[]string{"deploy"}, "", "this machine", 0},
		{[]string{"Bad/Host"}, "", "not a machine name", 0},
		{[]string{"air", "--to", "imac/codex.1"}, "", "one session on air", 0},
		{[]string{"air"}, "", "has no machine token here", 0},
		{[]string{"air"}, thAir, "readable by other users", 0o644},
		{[]string{"air"}, thImac, "not a machine token bound to air", 0o600},
		{[]string{"air"}, "nobody" + "-" + strings.Repeat("n", 26), "do not accept", 0o600},
	} {
		os.Remove(filepath.Join(dir, "air"))
		if tc.file != "" {
			os.WriteFile(filepath.Join(dir, "air"), []byte(tc.file+"\n"), tc.mode)
			os.Chmod(filepath.Join(dir, "air"), tc.mode)
		}
		code, out, e := run(t, "", append([]string{"token", "hand"}, tc.args...)...)
		if code == 0 || !strings.Contains(e, tc.want) {
			t.Errorf("%v: %d %q %q", tc.args, code, out, e)
		}
		if strings.Contains(out+e, thAir) || strings.Contains(out+e, thImac) {
			t.Errorf("%v: a token was printed", tc.args)
		}
	}
	if ms, _ := n.c.Inbox("air/claude.1", true); len(ms) != 0 {
		t.Errorf("a refused hand sent something: %d messages", len(ms))
	}
}

// hand takes the nodes' word, not the peer's: an answer sent with the shared token proves nothing.
func TestTokenHandBelievesOnlyWhatTheNodesSaw(t *testing.T) {
	n := tokenMachine(t, approved)
	lied, _ := n.c.Send(msg.Message{ID: msg.NewID(), From: "air/claude.1", To: "deploy/claude.9", Body: tokenInstalledTag + " air\ntrust me"})
	if ok, why := proveSent(n.c, "air", "air", lied.ID, 0); ok || !strings.Contains(why, "with the shared token") {
		t.Errorf("an answer sent with the shared token: %v %q", ok, why)
	}
	if ok, why := proveSent(n.c, "air", "air", msg.NewID(), 0); ok || !strings.Contains(why, "no node of this machine saw the answer") {
		t.Errorf("an answer no node saw: %v %q", ok, why)
	}
	own := &client.Client{URLs: n.c.URLs, Token: thAir}
	told, _ := own.Send(msg.Message{ID: msg.NewID(), From: "air/claude.1", To: "deploy/claude.9", Body: tokenInstalledTag + " air\nwith my own"})
	if ok, why := proveSent(n.c, "air", "air", told.ID, 0); !ok || !strings.Contains(why, `node n1 saw its answer `+told.ID+` sent with the machine token "air"`) {
		t.Errorf("an answer sent with air's token: %v %q", ok, why)
	}
	if ok, _ := proveSent(n.c, "air", "imac", told.ID, 0); ok {
		t.Error("another machine token's name proves nothing for this one")
	}
	old := httptest.NewServer(http.NotFoundHandler()) // a node from before token-seen
	t.Cleanup(old.Close)
	if ok, why := proveSent(&client.Client{URLs: []string{old.URL}, Token: thShared}, "air", "air", told.ID, 0); ok || !strings.Contains(why, "update them") {
		t.Errorf("old nodes: %v %q", ok, why)
	}
}

// A pipe link given by hand (made with the pipe's own client, p -e -f) works as well, also after a
// zsh paste put a backslash before its # and ~; any other backslash is refused before a fetch.
func TestTokenInstallTakesAPipeLinkEvenAsAShellPastedIt(t *testing.T) {
	n := tokenMachine(t, approved)
	held, _ := services(t)
	put := func() string {
		l, _, err := (&attach.Pipe{Base: os.Getenv("AIVOICEBUS_PIPE_URL")}).Put(context.Background(), []byte(thAir+"\n"), true)
		if err != nil {
			t.Fatal(err)
		}
		return l.URL
	}
	link := put()
	if code, _, e := run(t, "", "token", "install", strings.Replace(link, "#", `#\`, 1)); code == 0 || !strings.Contains(e, "backslash") {
		t.Errorf("a link changed some other way: %d %q", code, e)
	}
	if len(held()) != 1 {
		t.Fatal("a refused link was fetched")
	}
	pasted := strings.NewReplacer("#", `\#`, "~", `\~`).Replace(link)
	code, out, e := run(t, "", "token", "install", pasted, "--json")
	var res installResult
	if err := json.Unmarshal([]byte(out), &res); code != 0 || err != nil || !res.Installed || res.Name != "air" || res.Answer != "" {
		t.Fatalf("%d %q %q (%v)", code, out, e, err)
	}
	if tokenNow(t) != thAir {
		t.Error("not installed")
	}
	if ms, _ := n.c.Inbox("deploy/claude.9", true); len(ms) != 0 {
		t.Errorf("a link given by hand has nobody to answer: %d messages sent", len(ms))
	}
}

func TestTokenHandKeepsItOnANodeUnlessToldOtherwise(t *testing.T) {
	t.Setenv("AIVOICEBUS_FILE_VIA", "")
	if v := tokenVia(""); v != "node" {
		t.Errorf("default: %q", v)
	}
	t.Setenv("AIVOICEBUS_FILE_VIA", "pipe")
	if v := tokenVia(""); v != "pipe" {
		t.Errorf("the machine's choice for files: %q", v)
	}
	if v := tokenVia("node"); v != "node" {
		t.Errorf("--via wins: %q", v)
	}
}
