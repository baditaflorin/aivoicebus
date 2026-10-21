package outbox

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// rB is a second session the amended messages go to ("air/codex" is every codex session of that machine).
const rB = "air/codex.r3"

func amend(t *testing.T, c *client.Client, id, text string, o AmendOptions) (msg.Message, string, error) {
	t.Helper()
	var out bytes.Buffer
	m, err := Amend(c, id, text, o, &out)
	return m, out.String(), err
}

func inboxIDs(s *store.Store, me string) []string {
	var out []string
	for _, m := range s.Inbox(me, false) {
		out = append(out, m.ID)
	}
	return out
}

func TestAmendReplacesTheOriginalAtItsAudienceAndPriority(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o, err := s.Send(msg.Message{From: meID, To: "air/codex", Prio: msg.PrioUrgent, TTL: 7200, Body: "deploy to prod at noon"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(o.ID, reader1); err != nil { // reader1 read it; rB did not
		t.Fatal(err)
	}
	a, out, err := amend(t, c, o.ID, "deploy cancelled: do NOT deploy", AmendOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if a.To != o.To || a.Re != o.ID || a.Thread != o.Thread || a.Prio != msg.PrioUrgent || a.Kind != "" {
		t.Errorf("the same audience, thread and priority, a plain note: %+v", a)
	}
	if a.Exp == nil || o.Exp == nil || a.Exp.Sub(*o.Exp).Abs() > 5*time.Second {
		t.Errorf("it lives as long as the original would have: %v vs %v", a.Exp, o.Exp)
	}
	has(t, out, "replacing message "+o.ID+" to air/codex: \"deploy to prod at noon\"", "amended "+o.ID+" as "+a.ID+" thread "+o.Thread,
		"1 other session(s) had read "+o.ID, "the rest get only the new text", "undo: `aivoicebus retract "+a.ID+"`", "does NOT erase")
	if got := inboxIDs(s, rB); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("a reader that had not read the original is given the new text only: %v", got)
	}
	if got := s.Inbox(reader1, false); len(got) != 1 || got[0].ID != a.ID || got[0].Amends == nil || got[0].Amends.Read != o.ID {
		t.Errorf("a reader that had is told what it replaces: %+v", got)
	}
	// the new message is what retract with no id takes back: the original comes back for those who have not read either
	if res, err := retract(t, c, "", false, false, ""); err != nil || !strings.Contains(res, "retracted "+a.ID) {
		t.Fatalf("%v %s", err, res)
	}
	if got := inboxIDs(s, rB); !slices.Equal(got, []string{o.ID}) {
		t.Errorf("undoing the amendment gives the original back: %v", got)
	}
}

func TestAmendIsRefusedForWhatIsNotAPlainNoteOfThisMachine(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	theirs := put(t, s, elsewho, "air/codex", "their note")
	task, _ := s.Send(msg.Message{From: meID, To: "air/codex", Kind: msg.KindTask, Body: "do the thing"})
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	sealed, _ := msg.Seal("private words", k.PublicKey().Bytes())
	private := put(t, s, meID, "air/codex", sealed)
	gone := put(t, s, meID, "air/codex", "mistake")
	if _, err := s.Retract(gone.ID, meID, false); err != nil {
		t.Fatal(err)
	}
	for name, c2 := range map[string]struct{ id, want string }{
		"another machine's": {theirs.ID, "only that machine can amend it"},
		"a task":            {task.ID, "is a task, not a plain note"},
		"a private one":     {private.ID, "was sent --private"},
		"a withdrawn one":   {gone.ID, "was withdrawn"},
		"one not there":     {"0123456789", "was not found on the nodes"},
		"not an id":         {"nope", "not a message id"},
	} {
		if _, out, err := amend(t, c, c2.id, "corrected", AmendOptions{}); err == nil || !strings.Contains(err.Error(), c2.want) {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	if _, _, err := amend(t, c, theirs.ID, "  ", AmendOptions{}); err == nil || !strings.Contains(err.Error(), "corrected text") {
		t.Errorf("no text: %v", err)
	}
	if n := s.Stats().Messages; n != 4 {
		t.Errorf("nothing was sent by any of these: %d messages", n)
	}
}

func TestAmendTakesTheLatestVersionAndSaysSo(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o := put(t, s, meID, "air/codex", "deploy at noon")
	a1, _, err := amend(t, c, o.ID, "deploy at 13:00", AmendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a2, out, err := amend(t, c, o.ID, "deploy at 14:00", AmendOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	has(t, out, "message "+o.ID+" was already amended by "+a1.ID+": amending that one", "replacing message "+a1.ID)
	if a2.Re != a1.ID {
		t.Errorf("the second correction replaces the first, it does not sit beside it: re %s", a2.Re)
	}
	if got := inboxIDs(s, rB); !slices.Equal(got, []string{a2.ID}) {
		t.Errorf("%v", got)
	}
}

func TestAmendDryRunSendsNothing(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o := put(t, s, meID, "air/codex", "deploy at noon")
	before := s.Stats()
	m, out, err := amend(t, c, o.ID, "deploy at 13:00", AmendOptions{DryRun: true})
	if err != nil || m.ID != "" {
		t.Fatalf("%v %+v", err, m)
	}
	has(t, out, "dry run: nothing was sent", "no other session had read "+o.ID, "would get only the new text")
	if after := s.Stats(); after.Messages != before.Messages || after.Acks != before.Acks {
		t.Errorf("a dry run writes nothing: %+v -> %+v", before, after)
	}
}

func TestAmendWarnsWhenTheNodeDoesNotApplyAmendments(t *testing.T) {
	c, s := retractNode(t, meID, &msg.Health{Node: "old-node", Version: "0.11.0"})
	o := put(t, s, meID, "air/codex", "deploy at noon")
	_, out, err := amend(t, c, o.ID, "deploy at 13:00", AmendOptions{})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	has(t, out, "warning: the node that answered (old-node) does not apply amendments", "ordinary reply, beside the original")
}

func TestAmendSaysWhatToDoWhenTheNodesWillNotLinkIt(t *testing.T) {
	st, err := store.Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	st.SetSendLimit(1 << 20)
	inner := server.NewHandler(st, "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ack" {
			http.Error(w, "refused for the test", http.StatusForbidden)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", srv.URL)
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	t.Setenv("AIVOICEBUS_ME", meID)
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Chdir(t.TempDir())
	c, _ := client.FromEnv()
	c.Warn = &bytes.Buffer{}
	o := put(t, st, meID, "air/codex", "deploy at noon")
	a, out, err := amend(t, c, o.ID, "deploy at 13:00", AmendOptions{})
	if err == nil || a.ID == "" {
		t.Fatalf("the new text went out, and the failure is said: %+v %v\n%s", a, err, out)
	}
	for _, want := range []string{"went out as message " + a.ID, "ordinary reply beside the original", "aivoicebus ack amend:" + a.ID, "aivoicebus retract " + a.ID} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	var se *client.StatusError
	if !errors.As(err, &se) {
		t.Errorf("the refusal is kept for the caller: %v", err)
	}
}

func TestAmendOfAnUrgentMessageToManySessionsDoesNotAskTheFanOutQuestionAgain(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o, _ := s.Send(msg.Message{From: meID, To: "all", Prio: msg.PrioUrgent, Body: "stop scanning prod"})
	for i := 0; i < UrgentFanout+1; i++ { // six sessions seen lately
		s.InboxFor("box"+string(rune('a'+i))+"/claude.1", false, nil)
	}
	if _, out, err := amend(t, c, o.ID, "scanning prod is fine again", AmendOptions{}); err != nil {
		t.Errorf("the audience and the priority were chosen when it was sent: %v\n%s", err, out)
	}
	long := strings.Repeat("word ", BulkChars)
	if _, _, err := amend(t, c, o.ID, long, AmendOptions{}); err == nil || !strings.Contains(err.Error(), "puts about") {
		t.Errorf("the long-text guard still applies: %v", err)
	}
	if _, out, err := amend(t, c, o.ID, long, AmendOptions{Force: true}); err != nil {
		t.Errorf("and --force passes it: %v\n%s", err, out)
	}
}

func TestAmendRefusesATextThatLooksLikeACredential(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o := put(t, s, meID, "air/codex", "deploy at noon")
	if _, _, err := amend(t, c, o.ID, leakText(), AmendOptions{}); err == nil || !strings.Contains(err.Error(), "refused:") {
		t.Errorf("%v", err)
	}
	if n := s.Stats().Messages; n != 1 {
		t.Errorf("%d messages", n)
	}
}
