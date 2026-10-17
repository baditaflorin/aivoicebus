package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func inboxReq(t *testing.T, url string) (*http.Response, []msg.Message) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ms []msg.Message
	json.NewDecoder(resp.Body).Decode(&ms)
	return resp, ms
}

// A held read answers as soon as a message arrives, not at the end of its wait.
func TestAnInboxReadHeldOpenAnswersWhenAMessageArrives(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok"))
	defer srv.Close()
	base := srv.URL + "/v1/inbox?me=lap/claude.a1"
	resp, _ := inboxReq(t, base)
	g := resp.Header.Get(msg.HeaderGen)
	if _, err := strconv.ParseUint(g, 10, 64); err != nil {
		t.Fatalf("no %s header: %q", msg.HeaderGen, g)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.Send(msg.Message{From: "desk/claude.z9", To: "lap/claude.a1", Body: "now"})
	}()
	start := time.Now()
	_, ms := inboxReq(t, base+"&wait=20&gen="+g)
	if took := time.Since(start); len(ms) != 1 || took > 5*time.Second || took < 200*time.Millisecond {
		t.Fatalf("held read: %d messages after %s", len(ms), took)
	}
	// A reader that is behind (an old gen) is answered at once.
	start = time.Now()
	if _, ms := inboxReq(t, base+"&wait=20&gen=0"); len(ms) != 1 || time.Since(start) > 2*time.Second {
		t.Errorf("stale gen was held: %d after %s", len(ms), time.Since(start))
	}
	// Nothing arrives: the read ends after its wait (1s here), empty.
	resp, _ = inboxReq(t, base)
	start = time.Now()
	if _, ms := inboxReq(t, base+"&wait=1&gen="+resp.Header.Get(msg.HeaderGen)); time.Since(start) < 900*time.Millisecond || len(ms) != 1 {
		t.Errorf("an idle held read returned after %s with %d", time.Since(start), len(ms))
	}
}

func TestReceiptsEndpoint(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok", WithObserverToken("obs")))
	defer srv.Close()
	m, _ := s.Send(msg.Message{From: "desk/claude.z9", To: "lap", Body: "x", Kind: msg.KindTask})
	s.Ack(m.ID, "lap/claude.a1")
	for _, tok := range []string{"tok", "obs"} {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/receipts?id="+m.ID, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var r msg.Receipts
		json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		if resp.StatusCode != 200 || r.ID != m.ID || len(r.Reads) != 1 || r.Reads[0].By != "lap/claude.a1" {
			t.Errorf("%s: %d %+v", tok, resp.StatusCode, r)
		}
	}
	req, _ := http.NewRequest("GET", srv.URL+"/v1/receipts?id=0000000000", nil)
	req.Header.Set("Authorization", "Bearer tok")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown id: %d", resp.StatusCode)
	}
}
