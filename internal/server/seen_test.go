package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestTheNodeSaysWhichKindOfTokenLastActedAsAMachine(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "", WithNode("n1"), WithTokenSet(mustTokens(t, tokA+"\nlaptop-a:"+tokM+"\n")), WithObserverToken(tokO)))
	defer srv.Close()
	seen := func(host string) (int, msg.TokenSeen, string) {
		t.Helper()
		code, body := call(t, srv, tokO, "GET", "/v1/token/seen?host="+host, "") // the observer may ask: it is metadata
		var e msg.TokenSeen
		_ = json.Unmarshal([]byte(body), &e)
		return code, e, body
	}
	if code, e, _ := seen("laptop-a"); code != 200 || e.Node != "n1" || e.Host != "laptop-a" || e.Kind != "" || !e.At.IsZero() {
		t.Fatalf("nothing seen yet: %d %+v", code, e)
	}

	// the shared token sends as laptop-a: the node says so, with the message
	send := func(tok, from, body string) string {
		t.Helper()
		code, out := call(t, srv, tok, "POST", "/v1/send", fmt.Sprintf(`{"from":%q,"to":"imac/claude.1","body":%q}`, from, body))
		var m msg.Message
		if code != 200 || json.Unmarshal([]byte(out), &m) != nil {
			t.Fatalf("send: %d %q", code, out)
		}
		return m.ID
	}
	id1 := send(tokA, "laptop-a/claude.1", "with the shared token")
	if _, e, _ := seen("laptop-a"); e.Kind != "shared" || e.SendID != id1 || e.SendKind != "shared" || !strings.HasPrefix(e.SendName, "shared-") {
		t.Errorf("after a shared send: %+v", e)
	}

	// then its machine token: a plain request is the last one seen, the shared send stays the last send
	before := time.Now().Add(-time.Second)
	if code, _ := call(t, srv, tokM, "GET", "/v1/token", ""); code != 200 {
		t.Fatal(code)
	}
	if _, e, _ := seen("laptop-a"); e.Kind != "machine" || e.Name != "laptop-a" || e.At.Before(before) || e.SendID != id1 || e.SendKind != "shared" {
		t.Errorf("after a machine-token request: %+v", e)
	}
	id2 := send(tokM, "laptop-a/codex.2", "with its own token")
	_, e, body := seen("LAPTOP-A") // a machine name is not case-sensitive
	if e.SendID != id2 || e.SendKind != "machine" || e.SendName != "laptop-a" || e.SendAt.Before(before) {
		t.Errorf("after a machine-token send: %+v", e)
	}
	for _, tok := range []string{tokA, tokM, tokO} {
		if strings.Contains(body, tok) {
			t.Error("the answer never holds a token")
		}
	}

	// an inbox read as the machine counts; a read of the board does not name anyone
	if code, _ := call(t, srv, tokA, "GET", "/v1/inbox?me=laptop-a/claude.1", ""); code != 200 {
		t.Fatal(code)
	}
	if _, e, _ := seen("laptop-a"); e.Kind != "shared" || e.SendKind != "machine" {
		t.Errorf("after a shared inbox read: %+v", e)
	}

	// the observer acts as nobody, and a bad host is refused
	if code, _ := call(t, srv, tokO, "GET", "/v1/inbox?me=imac/claude.1", ""); code == 200 {
		t.Error("the observer reads no inbox")
	}
	if _, e, _ := seen("imac"); e.Kind != "" {
		t.Errorf("the observer is recorded as nobody: %+v", e)
	}
	for _, bad := range []string{"", "a/b", "x%20y", strings.Repeat("h", 64)} {
		if code, _, _ := seen(bad); code != 400 {
			t.Errorf("host %q: %d", bad, code)
		}
	}
	if code, _ := call(t, srv, "wrong-token-wrong-token-wrong", "GET", "/v1/token/seen?host=laptop-a", ""); code != 401 {
		t.Errorf("unauthenticated: %d", code)
	}
}

func TestTheSeenRecordIsBounded(t *testing.T) {
	b := newSeenBoard()
	clock := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	b.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	who := Principal{Name: "shared"}
	for i := range maxSeenHosts + 10 {
		b.note(fmt.Sprintf("h%d", i), who, "")
	}
	if len(b.hosts) != maxSeenHosts {
		t.Fatalf("%d hosts kept", len(b.hosts))
	}
	if e := b.get("h0"); e.Kind != "" {
		t.Errorf("the oldest goes first: %+v", e)
	}
	if e := b.get(fmt.Sprintf("h%d", maxSeenHosts+9)); e.Kind != "shared" {
		t.Errorf("the newest stays: %+v", e)
	}
}
