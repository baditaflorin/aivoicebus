package viewer

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestAutomatedIdentitiesAreNamedLikeScripts(t *testing.T) {
	for me, want := range map[string]bool{
		"laptop-a/join-probe":          true,
		"laptop-a/join-reader":         true,
		"rollout/agent":                true,
		"rollout-check/agent":          true,
		"x/check-060":                  true,
		"y/receipt-test":               true,
		"verify/claude":                true,
		"h/postdeploy-check":           true,
		"h/smoke.a1b2":                 true,
		"contests-macbook/claude.a1b2": false, // "contests" is not the word "test"
		"build-1/codex.q1w2":           false,
		"laptop/claude-work":           false,
		"contest/claude.a1b2":          false,
	} {
		if got := Automated(me); got != want {
			t.Errorf("Automated(%q) = %v, want %v", me, got, want)
		}
	}
}

func TestSessionsSayWhyTheyAreHiddenByDefault(t *testing.T) {
	roster := []msg.RosterEntry{{Me: "a/claude.1", Seen: t0}, {Me: "b/claude.2", Seen: t0}, {Me: "rollout/agent", Seen: t0}}
	ms := []msg.Message{{ID: "a000000001", Thread: "a000000001", From: "a/claude.1", To: "all", At: t0, Body: "hi"}}
	why := map[string]string{}
	for _, s := range Sessions(roster, ms) {
		why[s.Me] = s.Hidden
	}
	if why["a/claude.1"] != "" || why["b/claude.2"] != "no messages" || why["rollout/agent"] != "automation" {
		t.Errorf("%v", why)
	}
}

func fan(id, to string, at time.Time) msg.Message {
	return msg.Message{ID: id, Thread: id, From: "h/claude.3b50", To: to, At: at, Body: "From claude.3b50: the deploy is done"}
}

func TestOneMessageSentToSeveralSessionsIsOneConversation(t *testing.T) {
	ms := []msg.Message{
		fan("a000000001", "x/claude.e0ff", t0),
		fan("a000000002", "x/claude.a427", t0.Add(20*time.Second)),
		fan("a000000003", "y/claude.86e8", t0.Add(40*time.Second)),
		{ID: "a000000004", Thread: "a000000002", Re: "a000000002", From: "x/claude.a427", To: "h/claude.3b50", At: t0.Add(time.Minute), Body: "thanks"},
		fan("a000000005", "z/claude.late", t0.Add(10*time.Minute)), // the same words much later: another conversation
		{ID: "a000000006", Thread: "a000000006", From: "h/claude.3b50", To: "x/claude.e0ff", At: t0.Add(30 * time.Second), Body: "something else"},
	}
	cs := Conversations(ms, nil, "")
	if len(cs) != 3 {
		t.Fatalf("%d conversations: %+v", len(cs), cs)
	}
	var fanned *Conversation
	for i := range cs {
		if len(cs[i].Threads) > 0 {
			fanned = &cs[i]
		}
	}
	if fanned == nil || len(fanned.Threads) != 3 || fanned.Messages != 4 || len(fanned.Participants) != 4 || fanned.Thread != "a000000001" {
		t.Fatalf("%+v", fanned)
	}
	// a session that got one copy finds the conversation by its own thread
	if only := Conversations(ms, nil, "y/claude.86e8"); len(only) != 1 || len(only[0].Threads) != 3 {
		t.Errorf("%+v", only)
	}
}

func TestAConversationOfScriptsOnlyIsHiddenByDefault(t *testing.T) {
	ms := []msg.Message{
		{ID: "a000000001", Thread: "a000000001", From: "rollout/agent", To: "verify/claude", At: t0, Body: "ping"},
		{ID: "a000000002", Thread: "a000000002", From: "rollout/agent", To: "h/claude.a1b2", At: t0, Body: "your node was upgraded"},
	}
	hidden := map[string]bool{}
	for _, c := range Conversations(ms, nil, "") {
		hidden[c.Thread] = c.Hidden
	}
	if !hidden["a000000001"] || hidden["a000000002"] {
		t.Errorf("%v", hidden)
	}
}

func TestTheThreadOfAFanOutShowsTheMessageOnceWithEveryRecipient(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	at := time.Now()
	s.SetClock(func() time.Time { return at })
	a, _ := s.Send(msg.Message{From: "h/claude.3b50", To: "x/claude.e0ff", Body: "the deploy is done"})
	s.SetClock(func() time.Time { return at.Add(5 * time.Second) })
	s.Send(msg.Message{From: "h/claude.3b50", To: "x/claude.a427", Body: "the deploy is done"})
	s.SetClock(time.Now)
	s.Ack(a.ID, "x/claude.e0ff")
	v := &Viewer{st: s, title: "t"}
	hs := httptest.NewServer(v.Handler(HTTPOptions{Key: key}))
	defer hs.Close()
	var thread []ThreadMessage
	getJSON(t, hs, "/api/thread?id="+a.ID, &thread)
	if len(thread) != 1 || len(thread[0].Recipients) != 2 || thread[0].ReadBy != 1 {
		t.Errorf("%+v", thread)
	}
	var none []ThreadMessage
	if code := getJSON(t, hs, "/api/thread?id=ffffffffff", &none); code != 200 || none == nil {
		t.Errorf("an unknown thread is an empty list, not null: %d %v", code, none)
	}
}
