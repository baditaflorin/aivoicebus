package outbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	meID    = "me/claude.0001" // this session
	mePeer  = "me/claude.0002" // another session on this machine
	elsewho = "other/claude.9" // a session on another machine
	reader1 = "air/codex.r1"   // sessions that read what was sent
	reader2 = "imac/claude.r2"
)

// leakText is credential-shaped text built at run time, so no scanner sees a token in the source.
func leakText() string { return "key " + "gh" + "p_" + strings.Repeat("aB3dE", 8) + " thanks" }

// retractNode starts a node (answering /v1/health as health says, if given), points the process
// at it as me, and returns the client and the node's store.
func retractNode(t *testing.T, me string, health *msg.Health, opts ...server.Option) (*client.Client, *store.Store) {
	t.Helper()
	s, err := store.Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSendLimit(1 << 20)
	inner := server.NewHandler(s, "tok", opts...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if health != nil && r.URL.Path == "/v1/health" {
			json.NewEncoder(w).Encode(health)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", me)
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Chdir(t.TempDir())
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	c.Warn = &bytes.Buffer{}
	return c, s
}

func put(t *testing.T, s *store.Store, from, to, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{From: from, To: to, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// retract runs Retract and returns what it printed.
func retract(t *testing.T, c *client.Client, id string, leak, dry bool, as string) (string, error) {
	t.Helper()
	return retractWith(t, c, id, RetractOptions{Leak: leak, DryRun: dry, As: as})
}

func retractWith(t *testing.T, c *client.Client, id string, o RetractOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Retract(c, id, o, &out)
	return out.String(), err
}

func mustRetract(t *testing.T, c *client.Client, id string, leak, dry bool, as string) string {
	t.Helper()
	out, err := retract(t, c, id, leak, dry, as)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return out
}

func has(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

func lacks(t *testing.T, out string, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(out, b) {
			t.Errorf("must not contain %q:\n%s", b, out)
		}
	}
}

func TestRetractNamesTheMessageHidesItAndSaysWhatItCannotDo(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, reader1, "deploy window moved to 5pm")

	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "message "+m.ID+" to "+reader1+`: "deploy window moved to 5pm"`, "retracted "+m.ID,
		"no other session had read it",
		"This hides the text from inboxes and threads on nodes that apply retractions, and from the dashboard once it runs the matching release.", "It does NOT erase it", "node logs, exports and the dashboard archive", "rotate it")
	lacks(t, out, "tells sessions", "rotate that credential", "flagged")
	// the order matters: which message comes before what was done to it
	if strings.Index(out, "deploy window") > strings.Index(out, "retracted "+m.ID) {
		t.Errorf("the message is named before it is acted on:\n%s", out)
	}
	if got := s.InboxFor(reader1, false, nil); len(got) != 0 {
		t.Errorf("hidden from the unread inbox: %+v", got)
	}
	if th := s.Thread(m.ID); len(th) != 1 || th[0].Retracted == nil || strings.Contains(th[0].Body, "deploy window") {
		t.Errorf("a tombstone in the thread: %+v", th)
	}
}

func TestRetractCountsTheOtherSessionsThatHadReadItAndSaysWhichNodesTellThem(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, "all", "run the migration now")
	s.AckWith(msg.Ack{ID: m.ID, By: reader1})
	s.AckWith(msg.Ack{ID: m.ID, By: reader2})
	s.AckWith(msg.Ack{ID: m.ID, By: meID}) // the retractor does not count

	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "2 other session(s) had already read it", "Nodes that make notices tell them once; others do not",
		"Sessions that already read it are told once, by nodes that make notices, at that session's next inbox read: its next session start, or (for a leak, which is urgent) its next prompt in a running Claude Code session; a session that never reads its inbox again is not told.")
	lacks(t, out, "They are not told it was withdrawn")
	lacks(t, out, "no other session")
}

func TestRetractDryRunSaysTheSameAndWritesNothing(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, reader1, "hello")
	s.AckWith(msg.Ack{ID: m.ID, By: reader1})
	before := s.VV()["n1"]

	out := mustRetract(t, c, m.ID, false, true, "")
	has(t, out, "dry run: nothing was written", "would retract "+m.ID, "1 other session(s) had already read it", "does NOT erase it")
	lacks(t, out, "retracted "+m.ID)
	if s.VV()["n1"] != before || s.Thread(m.ID)[0].Retracted != nil || s.Stats().Retracted != 0 {
		t.Error("a dry run changes nothing")
	}
	if got := s.InboxFor(reader1, true, nil); len(got) != 1 || got[0].Retracted != nil {
		t.Errorf("still there: %+v", got)
	}
}

func TestRetractAgainSaysAlreadyAndWritesNothing(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, reader1, "oops")
	mustRetract(t, c, m.ID, false, false, "")
	n := s.VV()["n1"]

	out := mustRetract(t, c, m.ID, false, false, "") // the exit status is zero: it is already what was asked
	has(t, out, "already retracted by "+meID, "; nothing to do", "does NOT erase it")
	lacks(t, out, "had already read it") // nothing was asked of the node, so there is nothing new to count
	if s.VV()["n1"] != n {
		t.Error("nothing is written twice")
	}
}

func TestAnAlreadyRetractedMessageCanStillBeFlaggedAsALeak(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, reader1, "oops")
	mustRetract(t, c, m.ID, false, false, "")

	out := mustRetract(t, c, m.ID, true, false, "")
	has(t, out, "retracted without saying it held a credential", "flagged "+m.ID+" as having held a credential", "rotate that credential now")
	if r := s.Thread(m.ID)[0].Retracted; r == nil || !r.Leak {
		t.Errorf("%+v", r)
	}
	n := s.VV()["n1"]
	out = mustRetract(t, c, m.ID, true, false, "")
	has(t, out, "already retracted by", "(flagged as having held a credential)")
	if s.VV()["n1"] != n {
		t.Error("flagging twice writes once")
	}
}

func TestACredentialLookingMessageIsRetractedAsALeakWithoutBeingPrinted(t *testing.T) {
	secret := "gh" + "p_" + strings.Repeat("aB3dE", 8)
	for name, dry := range map[string]bool{"real": false, "dry run": true} {
		t.Run(name, func(t *testing.T) {
			c, s := retractNode(t, meID, nil)
			m := put(t, s, meID, reader1, leakText()) // stored directly: the node's own backstop would have refused it

			out := mustRetract(t, c, m.ID, false, dry, "")
			has(t, out, "message "+m.ID+" to "+reader1+": "+withheld, "the text looks like a GitHub token", "(--leak)")
			lacks(t, out, secret, secret[:20], "aB3dE", "thanks", `"key `)
			if dry {
				has(t, out, "would retract "+m.ID+", flagged as having held a credential")
				return
			}
			has(t, out, "retracted "+m.ID+", flagged as having held a credential", "rotate that credential now")
			r := s.Thread(m.ID)[0].Retracted
			if r == nil || !r.Leak {
				t.Fatalf("recorded as a leak: %+v", r)
			}
			if b := s.Thread(m.ID)[0].Body; strings.Contains(b, secret) || !strings.Contains(b, "may have held a credential") {
				t.Errorf("the tombstone: %q", b)
			}
		})
	}
	t.Run("a weak shape counts too", func(t *testing.T) {
		c, s := retractNode(t, meID, nil)
		m := put(t, s, meID, reader1, "the password: Xk9mQ2vL8nR4 for staging")
		out := mustRetract(t, c, m.ID, false, false, "")
		has(t, out, withheld, "retracting it as one that held a credential")
		lacks(t, out, "Xk9mQ2vL8nR4", "staging")
	})
	t.Run("a secret this machine holds counts, by name only", func(t *testing.T) {
		c, s := retractNode(t, meID, nil)
		const tok = "wUrKjC9xQrlSj7o0uzjugOIwbCxn6GMNLkApMfF9"
		dir := os.Getenv("HOME") + "/.config/aivoicebus"
		os.MkdirAll(dir, 0o700)
		os.WriteFile(dir+"/observer-token", []byte(tok+"\n"), 0o600)
		m := put(t, s, meID, reader1, "use "+tok+" for the dashboard")
		out := mustRetract(t, c, m.ID, false, false, "")
		has(t, out, withheld, "secret from this machine's aivoicebus config")
		lacks(t, out, tok, "for the dashboard")
	})
	t.Run("--leak forces it for text no detector knows, and the text is not printed either", func(t *testing.T) {
		c, s := retractNode(t, meID, nil)
		m := put(t, s, meID, reader1, "the vault pin is four two seven")
		out := mustRetract(t, c, m.ID, true, false, "")
		has(t, out, "message "+m.ID+" to "+reader1+": "+withheldFlagged, "retracted "+m.ID+", flagged", "rotate that credential now")
		lacks(t, out, "the text looks like", "vault pin", "four two seven")
		out = mustRetract(t, c, put(t, s, meID, reader1, "another pin, five six").ID, true, true, "")
		has(t, out, withheldFlagged, "would retract")
		lacks(t, out, "another pin")
	})
}

func TestRetractOnlyTakesBackWhatThisMachineSent(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, elsewho, "all", "not mine")
	n := s.VV()["n1"]
	out, err := retract(t, c, m.ID, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "was sent from other; this machine is me; only that machine can retract it") {
		t.Fatalf("%v", err)
	}
	has(t, out, `message `+m.ID+` to all: "not mine"`) // it still says which message it looked at
	if s.VV()["n1"] != n || s.Stats().Retracted != 0 {
		t.Error("nothing is written for another machine's message")
	}
}

func TestAnotherSessionOfTheSameMachineMayRetractWithANote(t *testing.T) {
	c, s := retractNode(t, mePeer, nil)
	m := put(t, s, meID, reader1, "from the session that has since restarted")
	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "note: retracting as "+mePeer+" a message sent as "+meID, "retracted "+m.ID)
	if r := s.Thread(m.ID)[0].Retracted; r == nil || r.By != mePeer {
		t.Errorf("%+v", r)
	}
}

func TestAsNeedsTheSharedTokenAndAMachineOnTheSendersHost(t *testing.T) {
	s, err := store.Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSendLimit(1 << 20)
	const shared = "shared-token-0123456789-0123456789"
	const machine = "machine-token-air-0123456789-0123"
	const observer = "observer-token-0123456789-012345"
	ts := server.NewTokenSet("")
	ts.Add(machine, server.Principal{Name: "air", Host: "air"})
	srv := httptest.NewServer(server.NewHandler(s, shared, server.WithTokenSet(ts), server.WithObserverToken(observer)))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_ME", "air/claude.1") // an operator on another machine than the sender's
	t.Chdir(t.TempDir())
	as := func(token string) *client.Client {
		t.Setenv("AIVOICEBUS_TOKEN", token)
		c, _ := client.FromEnv()
		c.Warn = &bytes.Buffer{}
		return c
	}
	m := put(t, s, "laptop/claude.a1b2", "all", "sent from the Mac")

	// no --as: this machine is not the sender's
	_, err = retract(t, as(shared), m.ID, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "only that machine can retract it") {
		t.Errorf("%v", err)
	}
	// --as with a machine token: it can only act as its own machine
	_, err = retract(t, as(machine), m.ID, false, false, "laptop/claude.dead")
	if err == nil || !strings.Contains(err.Error(), "--as needs the shared token: this machine's token can only act as air/") {
		t.Errorf("%v", err)
	}
	_, err = retract(t, as(observer), m.ID, false, false, "laptop/claude.dead")
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("%v", err)
	}
	// --as on a machine that is not the sender's: refused before asking a node
	_, err = retract(t, as(shared), m.ID, false, false, "imac/claude.7")
	if err == nil || !strings.Contains(err.Error(), "--as imac/claude.7 is on imac, but message "+m.ID+" was sent from laptop") {
		t.Errorf("%v", err)
	}
	if _, err = retract(t, as(shared), m.ID, false, false, "not an identity\x00"); err == nil || !strings.Contains(err.Error(), "--as: not an identity") {
		t.Errorf("%v", err)
	}
	if s.Stats().Retracted != 0 {
		t.Fatal("none of those wrote anything")
	}
	// the shared token declares an identity on the sender's machine
	out := mustRetract(t, as(shared), m.ID, false, false, "laptop/claude.dead")
	has(t, out, "note: retracting as laptop/claude.dead a message sent as laptop/claude.a1b2", "retracted "+m.ID)
	if r := s.Thread(m.ID)[0].Retracted; r == nil || r.By != "laptop/claude.dead" {
		t.Errorf("%+v", r)
	}
}

func TestRetractRefusesWhatIsNotAMessageOrIsNotThere(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	n := s.VV()["n1"]
	for _, id := range []string{"nope", "3F2A9C01DE", "retract:3f2a9c01de", "../../x", strings.Repeat("a", 11)} {
		if out, err := retract(t, c, id, false, false, ""); err == nil || !strings.Contains(err.Error(), "is not a message id (ten lowercase hex digits)") || out != "" {
			t.Errorf("%q: %v %q", id, err, out)
		}
	}
	if _, err := retract(t, c, "0123456789", false, false, ""); err == nil || !strings.Contains(err.Error(), "was not found on the nodes (a typo, or it has not reached them yet)") {
		t.Errorf("%v", err)
	}
	if s.VV()["n1"] != n {
		t.Error("nothing was written")
	}
}

func TestRetractFindsAMessageThatOnlyOneNodeHoldsYet(t *testing.T) {
	var urls []string
	var stores []*store.Store
	for _, n := range []string{"a", "b"} { // no replication between them: b alone has the message
		s, _ := store.Open(t.TempDir(), n)
		srv := httptest.NewServer(server.NewHandler(s, "tok"))
		t.Cleanup(srv.Close)
		urls = append(urls, srv.URL)
		stores = append(stores, s)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", meID)
	t.Chdir(t.TempDir())
	warn := &bytes.Buffer{}
	c := &client.Client{URLs: urls, Token: "tok", Warn: warn}
	m := put(t, stores[1], meID, "all", "only on b so far")

	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, `"only on b so far"`, "retracted "+m.ID)
	if r := stores[1].Thread(m.ID)[0].Retracted; r == nil {
		t.Error("the node that holds it applied it")
	}
	if stores[0].VV()["a"] != 0 {
		t.Error("the node that does not hold it recorded nothing")
	}
}

func TestOnlyOneLineOfTheBodyIsShownAndNoControlCharacters(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	body := "first line\n\x1b[31mred\x1b[0m\u202eRTL\ttabbed " + strings.Repeat("long ", 40)
	m := put(t, s, meID, reader1, body)
	out := mustRetract(t, c, m.ID, false, true, "")
	for _, bad := range []string{"\x1b", "\u202e", "\t"} {
		lacks(t, out, bad)
	}
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "message "+m.ID) {
			line = l
		}
	}
	has(t, line, `"first line [31mred[0mRTL tabbed long long`, `…"`)
	if n := len([]rune(strings.TrimPrefix(line, "message "+m.ID+" to "+reader1+": "))); n > excerptRunes+4 {
		t.Errorf("%d runes: %q", n, line)
	}
}

func TestOneLineCutsOnRuneBoundariesAndCollapsesSpace(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"  a \n\n b\t\tc  ", 20, "a b c"},
		{"héllo wörld", 5, "héllo…"},
		{"héllo", 5, "héllo"},
		{"a\x00b\x07c", 9, "abc"},
		{"\xff\xfeok", 9, "\ufffd\ufffdok"},
		{"", 5, ""},
	} {
		if got := oneLine(tc.in, tc.n); got != tc.want {
			t.Errorf("oneLine(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestNodesBeforeTheReleaseAreNamedAsStillDeliveringIt(t *testing.T) {
	health := &msg.Health{Node: "node-a", Version: "0.0.7", Peers: []msg.PeerHealth{
		{URL: "http://a.test:7777", OK: true, Version: "0.1.0"},
		{URL: "http://b.test:7777", OK: true, Version: "0.0.6"},
		{URL: "http://c.test:7777", OK: false, Version: "0.0.5"}, // unreachable: what it runs is not known now
		{URL: "http://d.test:7777", OK: true, Version: ""},       // does not say
	}}
	c, s := retractNode(t, meID, health)
	m := put(t, s, meID, reader1, "hello")
	out := mustRetract(t, c, m.ID, false, false, "")
	has(t, out, "node node-a (0.0.7) still delivers it: it does not apply retractions until it is upgraded",
		"the node at http://b.test:7777 (0.0.6) still delivers it: it does not apply retractions until it is upgraded")
	lacks(t, out, "a.test", "c.test", "d.test")
	if n := strings.Count(out, "still delivers it"); n != 2 {
		t.Errorf("%d lines:\n%s", n, out)
	}
	// and none when every node is new
	c, s = retractNode(t, meID, &msg.Health{Node: "node-a", Version: "0.7.0", Peers: []msg.PeerHealth{{URL: "http://a.test", OK: true, Version: "0.7.3"}}})
	m = put(t, s, meID, reader1, "hello again")
	lacks(t, mustRetract(t, c, m.ID, false, false, ""), "still delivers")
}

func TestNodeTooOldForStillNamesTheFirstOldOne(t *testing.T) {
	c, _ := retractNode(t, meID, &msg.Health{Node: "n1", Version: "0.0.7", Peers: []msg.PeerHealth{{URL: "http://p.test", OK: true, Version: "0.0.8"}}})
	if got := nodeTooOldFor(c, msg.SelectorSince); got != "node n1 (0.0.7)" {
		t.Errorf("%q", got)
	}
	if got := nodeTooOldFor(c, msg.RetractSince); got != "node n1 (0.0.7)" {
		t.Errorf("%q", got)
	}
}

// oldNode answers as a release that predates retraction does: it serves the message in its thread and says
// which release it is, has no /v1/retract (404) and takes whatever /v1/ack it is given.
type oldNode struct {
	mu   sync.Mutex
	acks []msg.AckRequest
}

func (o *oldNode) taken() []msg.AckRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]msg.AckRequest(nil), o.acks...)
}

func startOldNode(t *testing.T, m msg.Message, version string, peers ...msg.PeerHealth) (*oldNode, string) {
	t.Helper()
	o := &oldNode{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/thread":
			json.NewEncoder(w).Encode([]msg.Message{m})
		case "/v1/health":
			json.NewEncoder(w).Encode(msg.Health{Node: "old1", Version: version, Peers: peers})
		case "/v1/ack":
			var a msg.AckRequest
			if json.NewDecoder(r.Body).Decode(&a) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			o.mu.Lock()
			o.acks = append(o.acks, a)
			o.mu.Unlock()
			w.Write([]byte(`{"ok":true}`))
		default:
			http.NotFound(w, r) // /v1/retract, /v1/token
		}
	}))
	t.Cleanup(srv.Close)
	return o, srv.URL
}

func TestAMeshOfOldNodesTakesThePlainAckAndSaysNoNodeAppliesRetractionsYet(t *testing.T) {
	m := msg.Message{ID: "3f2a9c01de", From: meID, To: reader1, Thread: "3f2a9c01de", Body: "an old node serves this text"}
	for _, leak := range []bool{false, true} {
		o, url := startOldNode(t, m, "0.0.7")
		t.Setenv("HOME", t.TempDir())
		t.Setenv("AIVOICEBUS_ME", meID)
		t.Chdir(t.TempDir())
		warn := &bytes.Buffer{}
		c := &client.Client{URLs: []string{url}, Token: "tok", Warn: warn}

		out := mustRetract(t, c, m.ID, leak, false, "")
		wantID := "retract:" + m.ID
		if leak {
			wantID = "retract-leak:" + m.ID
		}
		if got := o.taken(); len(got) != 1 || got[0].ID != wantID || got[0].By != meID || got[0].Via != "" || got[0].Chars != 0 {
			t.Errorf("leak=%v: the carrier is an ordinary ack, via empty: %+v", leak, got)
		}
		has(t, out, "recorded the retraction of "+m.ID, "as a plain ack", "may predate retractions and not apply it",
			"no node of this mesh applies retractions yet, so nothing hides the message until one does",
			"how many other sessions had read it: unknown", "node old1 (0.0.7) still delivers it: it does not apply retractions until it is upgraded",
			"Where a node applies it, this hides the text from inboxes and threads, and from the dashboard once it runs the matching release. Sessions that already read it are told once, by nodes that make notices, at that session's next inbox read: its next session start, or (for a leak, which is urgent) its next prompt in a running Claude Code session; a session that never reads its inbox again is not told. It does NOT erase it")
		lacks(t, out, "\nThis hides", "no node applies retractions yet")
		lacks(t, out, "no other session had read it")
		if !strings.Contains(warn.String(), "only 1 node(s) took the write") {
			t.Errorf("one node taking it is said: %q", warn.String())
		}
		if leak {
			has(t, out, "flagged as having held a credential", "rotate that credential now")
		}
	}
}

func TestADryRunAgainstAnOldNodeSaysItCannotSayAndWritesNothing(t *testing.T) {
	m := msg.Message{ID: "3f2a9c01de", From: meID, To: reader1, Body: "hi"}
	o, url := startOldNode(t, m, "0.0.7")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", meID)
	t.Chdir(t.TempDir())
	c := &client.Client{URLs: []string{url}, Token: "tok", Warn: &bytes.Buffer{}}
	out := mustRetract(t, c, m.ID, false, true, "")
	has(t, out, "dry run: nothing was written", "No node that answered could say what a real run would do", "record the retraction as a plain ack", "unknown")
	if len(o.taken()) != 0 {
		t.Error("nothing is written")
	}
}

func TestARetractionThatIsRefusedOrRateLimitedSaysSoAndWritesNothing(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	s.SetRetractLimit(1)
	a := put(t, s, meID, reader1, "one")
	b := put(t, s, meID, reader1, "two")
	mustRetract(t, c, a.ID, false, false, "")
	out, err := retract(t, c, b.ID, false, false, "")
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limit") {
		t.Fatalf("%v", err)
	}
	has(t, out, "message "+b.ID) // still named
	lacks(t, out, "retracted "+b.ID)
	if s.Thread(b.ID)[0].Retracted != nil {
		t.Error("not retracted")
	}
}

func TestRetractWithNoIdTakesBackTheLastMessageSentAndNamesItFirst(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	first, err := (func() (msg.Message, error) {
		p, err := Prepare(c, Request{To: reader1, Body: "the first one"}, os.Stderr)
		if err != nil {
			return msg.Message{}, err
		}
		return p.Send(c)
	})()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Prepare(c, Request{To: reader2, Body: "the second one"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.Send(c)
	if err != nil {
		t.Fatal(err)
	}

	out := mustRetract(t, c, "", false, false, "")
	has(t, out, "last message sent by this session: "+second.ID+" to "+reader2+`: "the second one"`, "retracted "+second.ID)
	if s.Thread(second.ID)[0].Retracted == nil || s.Thread(first.ID)[0].Retracted != nil {
		t.Error("only the last one")
	}
	out = mustRetract(t, c, "", false, false, "") // the memory still says the second: it is already retracted
	has(t, out, "already retracted")
}

func TestRetractWithNoIdAndNothingRememberedAsksForAnId(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	put(t, s, meID, reader1, "sent by a tool that does not remember") // the store has it; the memory does not
	out, err := retract(t, c, "", false, false, "")
	if err == nil || !strings.Contains(err.Error(), "give the message id") || out != "" {
		t.Errorf("%v %q", err, out)
	}
}

func TestAMessageRepliedToAfterItWasWithdrawnIsWarnedAbout(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	m := put(t, s, reader1, meID, "question?")
	s.Retract(m.ID, reader1, false)
	var warn strings.Builder
	p, err := Prepare(c, Request{Re: m.ID, Body: "answer"}, &warn)
	if err != nil || p.Msg.To != reader1 {
		t.Fatalf("a reply to a withdrawn message still goes back to its sender: %+v %v", p.Msg, err)
	}
	has(t, warn.String(), "warning: message "+m.ID+" was withdrawn by "+msg.TombstoneWho(reader1))
	warn.Reset()
	if _, err := Prepare(c, Request{Re: m.ID, To: "x/y", Body: "answer"}, &warn); err != nil {
		t.Fatal(err)
	}
	has(t, warn.String(), "was withdrawn") // also when the target is given
	ok := put(t, s, reader1, meID, "another")
	warn.Reset()
	Prepare(c, Request{Re: ok.ID, Body: "answer"}, &warn)
	lacks(t, warn.String(), "withdrawn")
}

func TestPlanRecordsWhatLooksLikeACredentialButIsOnlyWarnedAbout(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	var warn strings.Builder
	p, err := Prepare(c, Request{To: reader1, Body: "the password: Xk9mQ2vL8nR4 for staging"}, &warn)
	if err != nil || len(p.Suspect) != 1 || p.Suspect[0] != "password or token value" {
		t.Fatalf("%v %+v", err, p.Suspect)
	}
	p, _ = Prepare(c, Request{To: reader1, Body: "plain words"}, &warn)
	if len(p.Suspect) != 0 {
		t.Errorf("%+v", p.Suspect)
	}
}

func TestARefusedCredentialSaysNothingWasSent(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	if _, err := Prepare(c, Request{To: reader1, Body: leakText()}, os.Stderr); err == nil || !strings.HasSuffix(err.Error(), "(nothing was sent)") {
		t.Errorf("the CLI's own check: %v", err)
	}
	if s.Stats().Messages != 0 {
		t.Fatal("nothing stored")
	}
	// the node's backstop: a token only the node knows, which this client's config does not hold
	s2, _ := store.Open(t.TempDir(), "n2")
	srv := httptest.NewServer(server.NewHandler(s2, "tok", server.WithObserverToken("observer-token-0123456789-012345")))
	t.Cleanup(srv.Close)
	c2 := &client.Client{URLs: []string{srv.URL}, Token: "tok", Warn: &bytes.Buffer{}}
	p, err := Prepare(c2, Request{To: reader1, Body: "for the dashboard: observer-token-0123456789-012345"}, os.Stderr)
	if err != nil {
		t.Fatalf("the client does not know that token: %v", err)
	}
	if _, err := p.Send(c2); err == nil || !strings.Contains(err.Error(), "looks like it contains") || !strings.HasSuffix(err.Error(), "(nothing was sent)") {
		t.Errorf("the node's refusal: %v", err)
	}
	if s2.Stats().Messages != 0 {
		t.Error("nothing stored")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config", "aivoicebus", sentFile)); err == nil {
		t.Error("a message that was not sent is not remembered as sent")
	}
}

func TestRetractWithNoReachableNodeSaysSoAndDoesNotPretend(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_ME", meID)
	for _, urls := range [][]string{{"http://127.0.0.1:1"}, {"http://127.0.0.1:1", "http://127.0.0.1:2"}} {
		c := &client.Client{URLs: urls, Token: "tok", Timeout: 500 * time.Millisecond, Warn: &bytes.Buffer{}}
		out, err := retract(t, c, "0123456789", false, false, "")
		if err == nil || !strings.Contains(err.Error(), "no node reachable") || out != "" {
			t.Errorf("%v: %v %q", urls, err, out)
		}
	}
}
