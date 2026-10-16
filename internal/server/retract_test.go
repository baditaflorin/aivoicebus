package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	rSender = "laptop-a/claude.a1b2"
	rOther  = "imac/claude.z9"
)

// retractNode is a node with a shared token, a machine token for laptop-a and an observer.
func retractNode(t *testing.T, opts ...Option) (*httptest.Server, *store.Store, *atomic.Bool) {
	t.Helper()
	s, err := store.Open(t.TempDir(), "n1")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSendLimit(1 << 20)
	ready := &atomic.Bool{}
	ready.Store(true)
	opts = append([]Option{WithTokenSet(mustTokens(t, tokA+"\nlaptop-a:"+tokM+"\n")), WithObserverToken(tokO), WithReady(ready.Load), WithNode("n1")}, opts...)
	srv := httptest.NewServer(NewHandler(s, "", opts...))
	t.Cleanup(srv.Close)
	return srv, s, ready
}

func sendAs(t *testing.T, s *store.Store, from, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{From: from, To: "all", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func retractBody(id, by string, extra string) string {
	return `{"id":"` + id + `","by":"` + by + `"` + extra + `}`
}

func decodeResult(t *testing.T, body string) msg.RetractResult {
	t.Helper()
	var r msg.RetractResult
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	return r
}

func TestRetractRoute(t *testing.T) {
	srv, s, ready := retractNode(t)
	post := func(tok, body string) (int, string) { return call(t, srv, tok, "POST", "/v1/retract", body) }

	t.Run("a shared token retracts as a session of the sender's machine", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words one")
		s.Ack(m.ID, "air/codex.q1")
		code, body := post(tokA, retractBody(m.ID, "laptop-a/claude.c3d4", ""))
		r := decodeResult(t, body)
		if code != 200 || !r.OK || r.State != "retracted" || r.Sender != rSender || r.ReadBy != 1 || r.Expired {
			t.Fatalf("%d %s", code, body)
		}
		if got := s.InboxFor("air/codex.z", false, nil); len(got) != 0 {
			t.Errorf("hidden at once: %+v", got)
		}
		if code, body := post(tokA, retractBody(m.ID, rSender, "")); code != 200 || decodeResult(t, body).State != "already" {
			t.Errorf("asking again: %d %s", code, body)
		}
		if code, body := post(tokA, retractBody(m.ID, rSender, `,"leak":true`)); code != 200 || decodeResult(t, body).State != "escalated" {
			t.Errorf("then with the credential flag: %d %s", code, body)
		}
	})

	t.Run("a machine token retracts as its own machine only", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words two")
		if code, body := post(tokM, retractBody(m.ID, rSender, "")); code != 200 {
			t.Errorf("its own machine: %d %s", code, body)
		}
		m2 := sendAs(t, s, rSender, "words three")
		if code, body := post(tokM, retractBody(m2.ID, rOther, "")); code != http.StatusForbidden || !strings.Contains(body, "laptop-a") {
			t.Errorf("claiming to be another machine: %d %s", code, body)
		}
		foreign := sendAs(t, s, rOther, "words four")
		if code, body := post(tokM, retractBody(foreign.ID, rSender, "")); code != http.StatusForbidden || !strings.Contains(body, "imac") {
			t.Errorf("another machine's message, as itself: %d %s", code, body)
		}
		if code, body := post(tokM, retractBody(foreign.ID, rOther, "")); code != http.StatusForbidden || !strings.Contains(body, "laptop-a") {
			t.Errorf("the token is for one machine: it cannot take on another machine's identity, even to retract that machine's own message: %d %s", code, body)
		}
		if got := s.Thread(foreign.ID)[0]; got.Retracted != nil {
			t.Error("and it was not retracted")
		}
	})

	t.Run("the observer, a missing token and a wrong one", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words five")
		for tok, want := range map[string]int{tokO: 403, "": 401, "wrong-wrong-wrong-wrong-wrong": 401} {
			if code, _ := post(tok, retractBody(m.ID, rSender, "")); code != want {
				t.Errorf("token %q: %d, want %d", tok, code, want)
			}
		}
		if s.Stats().Retracted != 2 { // the two above
			t.Errorf("%+v", s.Stats())
		}
	})

	t.Run("a node still recovering takes no writes", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words six")
		ready.Store(false)
		code, _ := post(tokA, retractBody(m.ID, rSender, ""))
		ready.Store(true)
		if code != http.StatusServiceUnavailable {
			t.Errorf("%d", code)
		}
	})

	t.Run("what is refused", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words seven")
		const upper = "ABCDEF0123" // a fixed id: a random one may hold no letter, and then upper-casing it changes nothing
		before := s.VV()["n1"]
		for name, c := range map[string]struct {
			body string
			want int
		}{
			"another machine than the sender":   {retractBody(m.ID, rOther, ""), 403},
			"a message this node does not hold": {retractBody("00aa11bb22", rSender, ""), 422},
			"no id":                             {retractBody("", rSender, ""), 400},
			"an id with a prefix":               {retractBody("retract:"+m.ID, rSender, ""), 400},
			"uppercase hex":                     {retractBody(upper, rSender, ""), 400},
			"nine digits":                       {retractBody(m.ID[:9], rSender, ""), 400},
			"no by":                             {retractBody(m.ID, "", ""), 400},
			"a control character in by":         {`{"id":"` + m.ID + `","by":"a\nb"}`, 400},
			"the quarantine placeholder as by":  {retractBody(m.ID, store.QuarantineBy, ""), 400},
			"not json":                          {`retract it`, 400},
			"an empty body":                     {``, 400},
		} {
			if code, body := post(tokA, c.body); code != c.want {
				t.Errorf("%s: %d %s, want %d", name, code, body, c.want)
			}
		}
		if code, _ := call(t, srv, tokA, "GET", "/v1/retract", ""); code != 400 {
			t.Errorf("GET: %d", code)
		}
		if s.VV()["n1"] != before {
			t.Error("nothing was written by any of them")
		}
	})

	t.Run("a dry run writes nothing and says what would happen", func(t *testing.T) {
		m := sendAs(t, s, rSender, "words eight")
		s.Ack(m.ID, "air/codex.q1")
		s.Ack(m.ID, "air/codex.q2")
		before := s.VV()["n1"]
		code, body := post(tokA, retractBody(m.ID, rSender, `,"dry_run":true`))
		r := decodeResult(t, body)
		if code != 200 || r.State != "dry-run" || r.Would != "retracted" || r.ReadBy != 2 || s.VV()["n1"] != before || s.Stats().Retracted != 2 {
			t.Fatalf("%d %s", code, body)
		}
		// a dry run is refused as a real one would be
		if code, _ := post(tokM, retractBody(m.ID, rOther, `,"dry_run":true`)); code != 403 {
			t.Errorf("%d", code)
		}
		if code, _ := post(tokA, retractBody("00aa11bb22", rSender, `,"dry_run":true`)); code != 422 {
			t.Errorf("%d", code)
		}
	})
}

func TestRetractIsRateLimitedPerIdentityAndOnlyForWhatItWrites(t *testing.T) {
	srv, s, _ := retractNode(t)
	s.SetRetractLimit(2)
	post := func(by, id string, extra string) (int, string) {
		return call(t, srv, tokA, "POST", "/v1/retract", retractBody(id, by, extra))
	}
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, sendAs(t, s, rSender, "m"+string(rune('a'+i))).ID)
	}
	for i := 0; i < 30; i++ { // none of this costs anything
		post(rOther, ids[0], "")                 // 403
		post(rSender, "00aa11bb22", "")          // 422
		post(rSender, ids[0], `,"dry_run":true`) // dry run
		post(rSender, "bad", "")                 // 400
	}
	for i := 0; i < 2; i++ {
		if code, body := post(rSender, ids[i], ""); code != 200 {
			t.Fatalf("%d: %d %s", i, code, body)
		}
		if code, _ := post(rSender, ids[i], ""); code != 200 { // already: free
			t.Fatal(code)
		}
	}
	req, _ := http.NewRequest("POST", srv.URL+"/v1/retract", strings.NewReader(retractBody(ids[2], rSender, "")))
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("the third: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if code, _ := post("laptop-a/claude.other", ids[2], ""); code != 200 {
		t.Errorf("another identity has its own allowance: %d", code)
	}
}

func TestAckOfARetractionIDGoesThroughTheSameChecksAsRetract(t *testing.T) {
	srv, s, ready := retractNode(t)
	ack := func(tok, id, by string, extra string) (int, string) {
		return call(t, srv, tok, "POST", "/v1/ack", `{"id":"`+id+`","by":"`+by+`"`+extra+`}`)
	}
	m := sendAs(t, s, rSender, "an old client retracts this")
	reader := "air/codex.q1"

	// the carrier an old client writes: aivoicebus ack retract:<id>, with a receipt it should not have
	if code, body := ack(tokA, "retract:"+m.ID, "laptop-a/claude.c3d4", `,"via":"hook","chars":999`); code != 200 || body != "{\"ok\":true}\n" {
		t.Fatalf("%d %q", code, body)
	}
	if got := s.InboxFor(reader, false, nil); len(got) != 0 {
		t.Errorf("an ack of a retraction id retracts: %+v", got)
	}
	var written *msg.Ack
	for _, r := range s.Since(map[string]uint64{}, 100) {
		if r.Ack != nil {
			written = r.Ack
		}
	}
	if written == nil || written.ID != "retract:"+m.ID || written.Via != "" || written.Chars != 0 {
		t.Errorf("it is written as the carrier, without a receipt: %+v", written)
	}
	n := s.VV()["n1"]
	if code, _ := ack(tokA, "retract:"+m.ID, rSender, ""); code != 200 || s.VV()["n1"] != n {
		t.Error("a repeat is accepted and writes nothing")
	}
	if code, _ := ack(tokA, "retract-leak:"+m.ID, rSender, ""); code != 200 || s.VV()["n1"] != n+1 || !s.Thread(m.ID)[0].Retracted.Leak {
		t.Error("the leak flavour escalates")
	}

	other := sendAs(t, s, rSender, "other")
	for name, c := range map[string]struct {
		tok, id, by string
		want        int
	}{
		"another machine":                {tokA, "retract:" + other.ID, rOther, 403},
		"a machine token as another one": {tokM, "retract:" + other.ID, rOther, 403},
		"an unknown message":             {tokA, "retract:00aa11bb22", rSender, 422},
		"a retraction of a retraction":   {tokA, "retract:retract:" + other.ID, rSender, 400},
		"the placeholder":                {tokA, "retract:quarantined", rSender, 400},
		"nothing after the prefix":       {tokA, "retract:", rSender, 400},
		"too short":                      {tokA, "retract:" + other.ID[:9], rSender, 400},
		"too long":                       {tokA, "retract-leak:" + other.ID + "0", rSender, 400},
		"the quarantine identity":        {tokA, "retract:" + other.ID, store.QuarantineBy, 400},
		"the observer":                   {tokO, "retract:" + other.ID, rSender, 403},
		"no token":                       {"", "retract:" + other.ID, rSender, 401},
	} {
		if code, body := ack(c.tok, c.id, c.by, ""); code != c.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.want)
		}
	}
	if s.Thread(other.ID)[0].Retracted != nil {
		t.Error("none of those retracted it")
	}
	ready.Store(false)
	if code, _ := ack(tokA, "retract:"+other.ID, rSender, ""); code != 503 {
		t.Errorf("%d", code)
	}
	ready.Store(true)

	// the allowance is shared with /v1/retract
	s.SetRetractLimit(1)
	third := sendAs(t, s, rSender, "third")
	if code, _ := ack(tokA, "retract:"+other.ID, "laptop-a/claude.lim", ""); code != 200 {
		t.Fatal(code)
	}
	if code, _ := call(t, srv, tokA, "POST", "/v1/retract", retractBody(third.ID, "laptop-a/claude.lim", "")); code != 429 {
		t.Errorf("a retraction by ack and one by /v1/retract draw on the same allowance: %d", code)
	}

	// an ordinary ack is unchanged
	if code, _ := ack(tokA, third.ID, reader, `,"via":"hook","chars":10`); code != 200 {
		t.Error("an ordinary ack")
	}
	if got := s.Stats(); got.Acks != 1 {
		t.Errorf("only the ordinary one is a read receipt: %+v", got)
	}
}

func TestSendClearsARetractedFieldTheSenderSet(t *testing.T) {
	srv, s, _ := retractNode(t)
	code, body := call(t, srv, tokA, "POST", "/v1/send", `{"from":"`+rSender+`","to":"all","body":"plain words","retracted":{"by":"imac/x","at":"2026-10-14T09:00:00Z","leak":true}}`)
	if code != 200 || strings.Contains(body, "retracted") {
		t.Fatalf("%d %s", code, body)
	}
	got := s.InboxFor("air/codex.q1", false, nil)
	if len(got) != 1 || got[0].Body != "plain words" || got[0].Retracted != nil {
		t.Errorf("%+v", got)
	}
}

func TestReadersSeeTheTombstoneOverHTTP(t *testing.T) {
	srv, s, _ := retractNode(t)
	m := sendAs(t, s, rSender, "text that was withdrawn")
	call(t, srv, tokA, "POST", "/v1/retract", retractBody(m.ID, rSender, ""))
	reader := "air/codex.q1"
	if code, body := call(t, srv, tokA, "GET", "/v1/inbox?me="+reader, ""); code != 200 || strings.TrimSpace(body) != "[]" && strings.TrimSpace(body) != "null" {
		t.Errorf("an unread inbox: %d %s", code, body)
	}
	code, body := call(t, srv, tokA, "GET", "/v1/inbox?me="+reader+"&all=1", "")
	var ms []msg.Message
	if code != 200 || json.Unmarshal([]byte(body), &ms) != nil || len(ms) != 1 || ms[0].Retracted == nil || ms[0].Retracted.By != rSender || strings.Contains(body, "text that was") {
		t.Errorf("--all: %d %s", code, body)
	}
	code, body = call(t, srv, tokO, "GET", "/v1/thread?id="+m.ID, "")
	if code != 200 || strings.Contains(body, "text that was") || !strings.Contains(body, "withdrawn by") {
		t.Errorf("a thread, even for the observer, is served with the tombstone: %d %s", code, body)
	}
	// the replication feed is the raw log, and still carries the message
	code, body = call(t, srv, tokO, "GET", "/v1/sync?vv=%7B%7D", "")
	if code != 200 || !strings.Contains(body, "text that was withdrawn") || !strings.Contains(body, `"retract:`+m.ID+`"`) {
		t.Errorf("the feed: %d %s", code, body)
	}
}

func TestHealthAndMetricsCountRetractionsAndTheOnesThatChangeNothing(t *testing.T) {
	srv, s, _ := retractNode(t)
	_, body := call(t, srv, tokA, "GET", "/v1/health", "")
	if strings.Contains(body, `"retracted"`) {
		t.Errorf("a node with none leaves the field out: %s", body)
	}
	m := sendAs(t, s, rSender, "x")
	m2 := sendAs(t, s, rSender, "y")
	call(t, srv, tokA, "POST", "/v1/retract", retractBody(m.ID, rSender, ""))
	s.AckWith(msg.Ack{ID: "retract:" + m2.ID, By: rOther}) // what replication could bring: a claim by another machine
	code, body := call(t, srv, tokO, "GET", "/v1/health", "")
	var h msg.Health
	if code != 200 || json.Unmarshal([]byte(body), &h) != nil || h.Retracted != 1 || h.Acks != 0 {
		t.Fatalf("%d %s", code, body)
	}
	_, metrics := call(t, srv, tokO, "GET", "/metrics", "")
	for _, want := range []string{"aivoicebus_retracted_messages 1\n", "aivoicebus_retractions_ineffective 1\n", "# TYPE aivoicebus_retracted_messages gauge"} {
		if !strings.Contains(metrics, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

func TestARetractionReplicatesAsAnOrdinaryAckAndHidesTheMessageOnThePeer(t *testing.T) {
	a, _ := store.Open(t.TempDir(), "a")
	b, _ := store.Open(t.TempDir(), "b")
	srvA := httptest.NewServer(NewHandler(a, "tok"))
	defer srvA.Close()
	m, _ := a.Send(msg.Message{From: rSender, To: "all", Body: "to be withdrawn"})
	if _, err := PullOnce(b, srvA.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if got := b.InboxFor("air/codex.q1", false, nil); len(got) != 1 {
		t.Fatal("it replicated")
	}
	if _, err := a.Retract(m.ID, rSender, false); err != nil {
		t.Fatal(err)
	}
	if n, err := PullOnce(b, srvA.URL, "tok"); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if got := b.InboxFor("air/codex.q1", false, nil); len(got) != 0 {
		t.Errorf("hidden on the peer: %+v", got)
	}
	if st := b.Stats(); st.Quarantined != 0 || st.Retracted != 1 {
		t.Errorf("%+v", st)
	}
	if time.Since(a.Since(map[string]uint64{}, 10)[1].Ack.At) > time.Minute {
		t.Error("the record carries the node's time")
	}
}
