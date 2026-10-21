package outbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// laneMesh is meshWith behind wrap (nil: none), with session "air/codex.b" registered with labels.
func laneMesh(t *testing.T, wrap func(http.Handler) http.Handler, labels string) (*client.Client, *store.Store) {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	var h http.Handler = server.NewHandler(s, "tok")
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", "me/claude.0001")
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Chdir(t.TempDir())
	s.InboxFor("air/codex.b", false, msg.ParseLabels(labels))
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

// withoutTheLane makes a node look like one before the test lane: its health lists no testlane.
func withoutTheLane(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		next.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		var kept []string
		for _, f := range h.Features {
			if f != msg.FeatureTestLane {
				kept = append(kept, f)
			}
		}
		h.Features = kept
		_ = json.NewEncoder(w).Encode(h)
	})
}

func TestATestMessageGetsATestIdAndAtMostTenMinutesAndSaysWhoItReaches(t *testing.T) {
	c, _ := laneMesh(t, nil, "test=on")
	p, err := Prepare(c, Request{To: "air/codex.b", Body: "calc 17*23", Test: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !msg.IsTestID(p.Msg.ID) || !p.Msg.IsTest() || p.Msg.TTL != int64(msg.TestLife/time.Second) {
		t.Errorf("a test id and the ten-minute life: %+v", p.Msg)
	}
	if p.Msg.Reach == nil || *p.Msg.Reach != 1 || !strings.Contains(strings.Join(p.Notes, " "), "test message") {
		t.Errorf("it counts the opted-in sessions it reaches and says it is a test: %+v %v", p.Msg.Reach, p.Notes)
	}
	p, err = Prepare(c, Request{To: "air/codex.b", Body: "short", Test: true, TTL: "5m"}, io.Discard)
	if err != nil || p.Msg.TTL != 300 || !p.Msg.IsTest() {
		t.Errorf("a shorter ttl stands: %+v %v", p.Msg, err)
	}
	if _, err := Prepare(c, Request{To: "air/codex.b", Body: "long", Test: true, TTL: "11m"}, io.Discard); err == nil || !strings.Contains(err.Error(), "at most 10m") {
		t.Errorf("a test message cannot live longer than ten minutes: %v", err)
	}
	if p, err := Prepare(c, Request{To: "air/codex.b", Body: "ordinary"}, io.Discard); err != nil || msg.IsTestID(p.Msg.ID) {
		t.Errorf("an ordinary message gets an ordinary id: %+v %v", p.Msg, err)
	}
}

func TestATestMessageIsRefusedWhenNobodyThereOptedInOrIsAround(t *testing.T) {
	c, _ := laneMesh(t, nil, "repo=alpha")
	_, err := Prepare(c, Request{To: "air/codex.b", Body: "calc 1+1", Test: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "opted in") || !strings.Contains(err.Error(), "AIVOICEBUS_TEST=1") {
		t.Errorf("nobody opted in: %v", err)
	}
	// opted in, but not seen for two hours: it will not be there in the ten minutes the message lives
	c, s := laneMesh(t, nil, "")
	past := time.Now().Add(-2 * time.Hour)
	s.SetClock(func() time.Time { return past })
	s.InboxFor("air/codex.b", false, msg.Labels{msg.TestLabel: msg.TestOn})
	s.SetClock(time.Now)
	if _, err := Prepare(c, Request{To: "air/codex.b", Body: "calc 1+1", Test: true}, io.Discard); err == nil || !strings.Contains(err.Error(), "seen in the last 30m") {
		t.Errorf("an opted-in session last seen two hours ago: %v", err)
	}
	// an ordinary message to the same session is not held to this
	if _, err := Prepare(c, Request{To: "air/codex.b", Body: "ordinary", Queue: true}, io.Discard); err != nil {
		t.Errorf("an ordinary message is not affected: %v", err)
	}
}

func TestATestMessageIsRefusedOnAMeshWithANodeThatPredatesTheLane(t *testing.T) {
	c, s := laneMesh(t, withoutTheLane, "test=on")
	_, err := Prepare(c, Request{To: "air/codex.b", Body: "calc 1+1", Test: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "predates the test lane") {
		t.Fatalf("it would be shown to everyone addressed and never purged there: %v", err)
	}
	if s.Stats().Messages != 0 {
		t.Error("nothing is sent")
	}
	// a reply to a test message is a test message, so it is refused there too, not sent as an ordinary one
	root := sendTestRoot(t, c)
	if _, err := Prepare(c, Request{To: "air/codex.b", Re: root.ID, Body: "answer"}, io.Discard); err == nil {
		t.Error("a reply to a test message must not go out as an ordinary one on a node that cannot hide it")
	}
}

func sendTestRoot(t *testing.T, c *client.Client) msg.Message {
	t.Helper()
	m, err := c.Send(msg.Message{ID: msg.NewTestID(), From: "air/codex.b", To: "me/claude.0001", Body: "a test root", TTL: 300})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAReplyToATestMessageStaysInTheLaneAndASignedDecisionDoesNot(t *testing.T) {
	c, _ := laneMesh(t, nil, "test=on")
	root := sendTestRoot(t, c)
	p, err := Prepare(c, Request{To: "air/codex.b", Re: root.ID, Body: "the answer", TargetSaid: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Msg.IsTest() || p.Msg.Re != root.ID {
		t.Errorf("a reply to a test message is one, with no flag: %+v", p.Msg)
	}
	// the one that cannot be a test (it is for the person) goes out as an ordinary message
	p, err = Prepare(c, Request{To: "air", Re: root.ID, Body: msg.ApprovalTag + " a task wants write access", TargetSaid: true}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if p.Msg.IsTest() || msg.IsTestID(p.Msg.ID) {
		t.Errorf("an approval request cannot hide in the lane: %+v", p.Msg)
	}
	// asked for as a test, it is refused
	if _, err := Prepare(c, Request{To: "air", Body: msg.ApprovalTag + " x", Test: true}, io.Discard); err == nil || !strings.Contains(err.Error(), "person") {
		t.Errorf("--test on a message for the person: %v", err)
	}
	if _, err := Prepare(c, Request{To: "me/claude.0001", Body: msg.LeaseBody("k", "n"), Kind: msg.KindLease, TTL: "5m", Test: true}, io.Discard); err == nil || !strings.Contains(err.Error(), "lease") {
		t.Errorf("--test on a lease: %v", err)
	}
}

func TestAReplyToATestMessageNeedsNoOptInOfItsReaderButASendDoes(t *testing.T) {
	c, _ := laneMesh(t, nil, "repo=alpha") // nobody opted in
	root, err := c.Send(msg.Message{ID: msg.NewTestID(), From: "air/codex.b", To: "me/claude.0001", Body: "a test root", TTL: 300})
	if err != nil {
		t.Fatal(err)
	}
	// the answer goes back to the one who wrote the test, who read it through the lane (a throwaway identity)
	p, err := Prepare(c, Request{To: "air/codex.b", Re: root.ID, Body: "the answer", TargetSaid: true}, io.Discard)
	if err != nil || !p.Msg.IsTest() {
		t.Errorf("a reply to a test message is sent, as a test, to a reader that did not opt in: %+v %v", p.Msg, err)
	}
	if _, err := Prepare(c, Request{To: "air/codex.b", Body: "a new test", Test: true}, io.Discard); err == nil {
		t.Error("a message sent as a test needs someone who opted in to read it")
	}
}

func TestSpanShortDropsTheZeroParts(t *testing.T) {
	for d, want := range map[time.Duration]string{10 * time.Minute: "10m", 30 * time.Minute: "30m", time.Hour: "1h", 90 * time.Minute: "1h30m", 45 * time.Second: "45s", 72 * time.Hour: "3d"} {
		if got := SpanShort(d); got != want {
			t.Errorf("SpanShort(%s) = %q, want %q", d, got, want)
		}
	}
}
