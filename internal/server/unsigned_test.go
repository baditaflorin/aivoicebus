package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// /v1/unsigned counts, per sender, what --require-signed would have refused in the window: no
// signature, or one that does not verify. Signed messages, probes and older messages are not counted.
func TestUnsignedReportCountsWhatRequireSignedWouldRefuse(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	now := time.Now()
	s.SetClock(func() time.Time { return now.Add(-10 * 24 * time.Hour) })
	s.Send(msg.Message{From: "old/claude.a", To: "all", Body: "unsigned, but before the window"})
	s.SetClock(func() time.Time { return now })

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed := msg.Message{ID: msg.NewID(), From: "lap/claude.a1", To: "all", Body: "signed"}
	signed.Sign(priv, nil)
	s.Send(signed)
	forged := signed
	forged.ID, forged.Body = msg.NewID(), "changed after signing"
	forged.Sign(priv, nil)
	forged.Body = "changed after signing, really"
	s.Send(forged)
	s.Send(msg.Message{From: "imac/codex.b", To: "all", Body: "one"})
	s.Send(msg.Message{From: "imac/codex.b", To: "all", Body: "two"})
	s.Send(msg.Message{From: "lap/" + msg.ProbeSenderAgent, To: "lap/" + msg.ProbeReaderAgent, Body: "probe"})

	srv := httptest.NewServer(NewHandler(s, "t", WithNode("n1"), WithRequireSigned(true)))
	defer srv.Close()
	get := func(q string) (int, msg.UnsignedReport) {
		req, _ := http.NewRequest("GET", srv.URL+"/v1/unsigned"+q, nil)
		req.Header.Set("Authorization", "Bearer t")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r msg.UnsignedReport
		json.NewDecoder(resp.Body).Decode(&r)
		return resp.StatusCode, r
	}
	code, r := get("")
	if code != 200 || r.Node != "n1" || !r.RequireSigned || r.Scanned != 4 {
		t.Fatalf("code %d, report %+v", code, r)
	}
	got := map[string]msg.UnsignedSender{}
	for _, e := range r.Senders {
		got[e.From] = e
	}
	if len(got) != 2 || got["imac/codex.b"].Unsigned != 2 || got["lap/claude.a1"].Bad != 1 || got["lap/claude.a1"].Unsigned != 0 {
		t.Errorf("senders: %+v", r.Senders)
	}
	if code, r = get("?days=11"); code != 200 || len(r.Senders) != 3 {
		t.Errorf("11 days reach the old one: %d %+v", code, r.Senders)
	}
	for _, bad := range []string{"?days=0", "?days=31", "?days=x"} {
		if code, _ := get(bad); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if !slices.Contains(msg.NodeFeatures(), msg.FeatureUnsigned) {
		t.Error("the node does not advertise unsigned-report")
	}
}
