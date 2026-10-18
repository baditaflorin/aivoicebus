package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// /v1/decide takes no token, so it must take nothing but the right one-time code for an open
// approval request, once.
func TestDecideTakesOnlyTheRightCodeOnce(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok-0123456789-0123456789-xx"))
	defer srv.Close()
	const code = "0123456789abcdef0123456789abcdef"
	req, _ := s.Send(msg.Message{ID: msg.NewID(), From: "lap/codex.worker", To: "desk/claude.a", Body: "[approval needed] run x\n" + msg.PhoneCheckLine(code)})
	plain, _ := s.Send(msg.Message{ID: msg.NewID(), From: "lap/codex.worker", To: "desk/claude.a", Body: "hello\n" + msg.PhoneCheckLine(code)})
	post := func(q string) (int, string) {
		r, err := http.Post(srv.URL+"/v1/decide?"+q, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b := new(strings.Builder)
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		b.Write(buf[:n])
		return r.StatusCode, b.String()
	}
	if r, _ := http.Get(srv.URL + "/v1/decide?id=" + req.ID + "&say=approve&code=" + code); r.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", r.StatusCode)
	}
	if c, _ := post("id=" + req.ID + "&say=approve&code=ffffffffffffffffffffffffffffffff"); c != http.StatusForbidden {
		t.Errorf("wrong code: %d", c)
	}
	if c, _ := post("id=" + plain.ID + "&say=approve&code=" + code); c != http.StatusNotFound {
		t.Errorf("not a request: %d", c)
	}
	if c, _ := post("id=" + req.ID + "&say=maybe&code=" + code); c != http.StatusBadRequest {
		t.Errorf("bad word: %d", c)
	}
	if c, body := post("id=" + req.ID + "&say=approve&code=" + code); c != http.StatusOK || !strings.Contains(body, "recorded: approve") {
		t.Fatalf("right code: %d %s", c, body)
	}
	if c, _ := post("id=" + req.ID + "&say=deny&code=" + code); c != http.StatusTooManyRequests {
		t.Errorf("a second decision: %d", c)
	}
	ms := s.Thread(req.ID)
	last := ms[len(ms)-1]
	if say, got, ok := msg.ParsePhoneDecision(last.Body); !ok || say != "approve" || got != code || last.From != msg.PhoneIdentity || last.To != "lap/codex.worker" {
		t.Errorf("recorded: %+v", last)
	}
}

// A question to a person takes only one of its choices from the phone.
func TestDecideTakesOnlyAQuestionsChoices(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok-0123456789-0123456789-xx"))
	defer srv.Close()
	const code = "0123456789abcdef0123456789abcdef"
	q, _ := s.Send(msg.Message{ID: msg.NewID(), From: "lap/claude.a", To: "lap/human", Body: msg.AskBody("deploy?", []string{"yes", "no"}, code)})
	post := func(say string) int {
		r, err := http.Post(srv.URL+"/v1/decide?id="+q.ID+"&code="+code+"&say="+say, "text/plain", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if c := post("approve"); c != http.StatusBadRequest {
		t.Errorf("not a choice: %d", c)
	}
	if c := post("no"); c != http.StatusOK {
		t.Errorf("a choice: %d", c)
	}
}
