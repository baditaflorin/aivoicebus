package client

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestCheckPinsTheFirstKeyAndFlagsAChangedOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, k1, _ := ed25519.GenerateKey(rand.Reader)
	_, k2, _ := ed25519.GenerateKey(rand.Reader)
	box, _ := ecdh.X25519().GenerateKey(rand.Reader)
	signed := func(k ed25519.PrivateKey, body string) msg.Message {
		m := msg.Message{ID: "3f2a9c01de", From: "lap/claude.a1", To: "desk/codex.b2", Body: body}
		m.Sign(k, box.PublicKey().Bytes())
		return m
	}
	tampered := signed(k1, "deploy")
	tampered.Body = "deploy prod"
	ms := []msg.Message{signed(k1, "hello"), signed(k2, "hello again"), {From: "old/claude.x", Body: "unsigned"}, tampered,
		{From: "lap/claude.a1", Body: "withdrawn", Retracted: &msg.Retraction{}}}
	Check(ms)
	want := []string{msg.VerifiedSigned, msg.VerifiedChanged, msg.VerifiedUnsigned, msg.VerifiedBad, ""}
	for i, w := range want {
		if ms[i].Verified != w {
			t.Errorf("message %d: %q, want %q", i, ms[i].Verified, w)
		}
	}
	if key, b, ok := config.KnownMachine("lap"); !ok || key != ms[0].Key || b == "" {
		t.Errorf("not pinned: %q %q %v", key, b, ok)
	}
	// A node cannot say a message was verified or private: Check decides afresh.
	lie := []msg.Message{{From: "x/y.z", Body: "hi", Verified: msg.VerifiedSigned, Private: true}}
	Check(lie)
	if lie[0].Verified != msg.VerifiedUnsigned || lie[0].Private {
		t.Errorf("a node's word was kept: %+v", lie[0])
	}
}

func TestCheckOpensWhatIsSealedForThisMachine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, box, err := config.MachineKeys()
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := msg.Seal("only for you", msg.BoxPublic(box))
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	notMine, _ := msg.Seal("for someone else", other.PublicKey().Bytes())
	ms := []msg.Message{{From: "a/b.c", Body: sealed}, {From: "a/b.c", Body: notMine}}
	Check(ms)
	if !ms[0].Private || ms[0].Body != "only for you" || ms[1].Private || !msg.IsSealed(ms[1].Body) {
		t.Errorf("%+v", ms)
	}
}
