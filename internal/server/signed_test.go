package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// --require-signed: an unsigned or forged message is refused; a signed one and a join probe go.
func TestRequireSignedRefusesUnsignedMessages(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t", WithRequireSigned(true)))
	defer srv.Close()
	send := func(m msg.Message) int {
		b, _ := json.Marshal(m)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/send", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer t")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signed := msg.Message{ID: msg.NewID(), From: "lap/claude.a1", To: "all", Body: "signed"}
	signed.Sign(priv, nil)
	if c := send(signed); c != http.StatusOK {
		t.Errorf("signed: %d", c)
	}
	if c := send(msg.Message{ID: msg.NewID(), From: "lap/claude.a1", To: "all", Body: "unsigned"}); c != http.StatusForbidden {
		t.Errorf("unsigned: %d", c)
	}
	forged := signed
	forged.ID, forged.Body = msg.NewID(), "changed after signing"
	if c := send(forged); c != http.StatusForbidden {
		t.Errorf("forged: %d", c)
	}
	if c := send(msg.Message{ID: msg.NewID(), From: "lap/" + msg.ProbeSenderAgent, To: "lap/" + msg.ProbeReaderAgent, Body: "probe"}); c != http.StatusOK {
		t.Errorf("probe: %d", c)
	}
}

// /metrics names what was refused and why, so a dashboard sees loops and brakes as they happen.
func TestMetricsCountRefusalsByReason(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "t", WithRequireSigned(true)))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/send", bytes.NewReader([]byte(`{"from":"a/b.c","to":"all","body":"unsigned"}`)))
	req.Header.Set("Authorization", "Bearer t")
	r, _ := srv.Client().Do(req)
	r.Body.Close()
	root, _ := s.Send(msg.Message{From: "a/b.c", To: "x/y.z", Body: "count: 1"})
	for i := 0; i < 3; i++ {
		s.Send(msg.Message{From: "x/y.z", To: "a/b.c", Re: root.ID, Body: fmt.Sprintf("number %d", 7+i)}) // the third is a loop
	}
	mr, _ := http.NewRequest("GET", srv.URL+"/metrics", nil)
	mr.Header.Set("Authorization", "Bearer t")
	resp, err := srv.Client().Do(mr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	for _, want := range []string{`aivoicebus_refused_total{reason="unsigned"} 1`, `aivoicebus_refused_total{reason="loop"} 1`, "aivoicebus_messages_last_hour 3"} {
		if !bytes.Contains(b.Bytes(), []byte(want)) {
			t.Errorf("metrics lack %q", want)
		}
	}
}
