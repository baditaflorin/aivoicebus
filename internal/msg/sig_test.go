package msg

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
)

func TestSignaturesCatchAnyChange(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	box, _ := ecdh.X25519().GenerateKey(rand.Reader)
	m := Message{ID: "3f2a9c01de", From: "lap/claude.a1", To: "desk/codex.b2", Re: "0123456789", Kind: KindTask, Prio: PrioUrgent, Body: "deploy"}
	m.Sign(priv, box.PublicKey().Bytes())
	if signed, valid := m.VerifySig(); !signed || !valid {
		t.Fatal("a fresh signature does not verify")
	}
	m.Thread, m.TTL = "anything", 99 // what a node fills in is not covered
	if _, valid := m.VerifySig(); !valid {
		t.Error("the thread or ttl broke the signature")
	}
	for name, change := range map[string]func(*Message){
		"body": func(x *Message) { x.Body = "deploy prod" },
		"from": func(x *Message) { x.From = "lap/claude.zz" },
		"to":   func(x *Message) { x.To = "all" },
		"kind": func(x *Message) { x.Kind = "" },
		"box":  func(x *Message) { x.Box = strings.Repeat("A", 43) + "=" },
	} {
		c := m
		change(&c)
		if _, valid := c.VerifySig(); valid {
			t.Errorf("a changed %s still verifies", name)
		}
	}
	if signed, _ := (Message{Body: "x"}).VerifySig(); signed {
		t.Error("an unsigned message counts as signed")
	}
}

func TestSealOnlyTheRecipientCanOpen(t *testing.T) {
	to, _ := ecdh.X25519().GenerateKey(rand.Reader)
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	sealed, err := Seal("the staging url is http://10.0.0.5", to.PublicKey().Bytes())
	if err != nil || !IsSealed(sealed) || strings.Contains(sealed, "10.0.0.5") {
		t.Fatalf("seal: %v %q", err, sealed)
	}
	if pt, err := Open(sealed, to.Bytes()); err != nil || pt != "the staging url is http://10.0.0.5" {
		t.Errorf("open: %q %v", pt, err)
	}
	if _, err := Open(sealed, other.Bytes()); err == nil {
		t.Error("another machine opened it")
	}
	bad := sealed[:len(sealed)-6] + "AAAAAA"
	if _, err := Open(bad, to.Bytes()); err == nil {
		t.Error("an altered sealed message opened")
	}
	if string(BoxPublic(to.Bytes())) != string(to.PublicKey().Bytes()) {
		t.Error("BoxPublic")
	}
}

func TestPhoneCodesMatchOnlyTheirHash(t *testing.T) {
	const code = "0123456789abcdef0123456789abcdef"
	body := "[approval needed] x\n" + PhoneCheckLine(code)
	if !PhoneCodeMatches(body, code) || PhoneCodeMatches(body, "ffffffffffffffffffffffffffffffff") || PhoneCodeMatches(body, "short") || PhoneCodeMatches("no line", code) {
		t.Error("PhoneCodeMatches")
	}
	if say, c, ok := ParsePhoneDecision(PhoneDecisionBody("deny", code)); !ok || say != "deny" || c != code {
		t.Error("ParsePhoneDecision")
	}
	if _, _, ok := ParsePhoneDecision("phone approve " + code + " extra"); ok {
		t.Error("extra words")
	}
}
