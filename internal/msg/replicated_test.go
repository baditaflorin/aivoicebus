package msg

import (
	"errors"
	"strings"
	"testing"
)

func TestTheReplicationCheckTakesWhatEveryReleaseWritesAndRefusesWhatNoneDoes(t *testing.T) {
	reach := 3
	bigReach, negReach := MaxReach+1, -1
	m := func(edit func(*Message)) Record {
		x := Message{ID: "0123456789", From: "imac/claude.a1b2", To: "all", Thread: "0123456789", Body: "hello"}
		edit(&x)
		return Record{Origin: "node-a", Seq: 1, Msg: &x}
	}
	a := func(edit func(*Ack)) Record {
		x := Ack{ID: "0123456789", By: "imac/claude.a1b2"}
		edit(&x)
		return Record{Origin: "node-a", Seq: 1, Ack: &x}
	}
	cases := []struct {
		name string
		r    Record
		bad  string // "" = must pass; else a word of the reason
	}{
		{"plain message", m(func(*Message) {}), ""},
		{"every optional field", m(func(x *Message) {
			x.Re, x.Prio, x.Kind, x.Reach = "89abcdef01", PrioUrgent, KindAnswer, &reach
		}), ""},
		{"a reply to a message this node never saw", m(func(x *Message) { x.Re, x.Thread = strings.Repeat("r", 64), strings.Repeat("t", 64) }), ""},
		{"fyi and question", m(func(x *Message) { x.Prio, x.Kind = PrioFYI, KindQuestion }), ""},
		{"a selector target", m(func(x *Message) { x.To = "@repo=aivoicebus,tool=claude|codex" }), ""},
		{"identities at the limit", m(func(x *Message) { x.From, x.To = strings.Repeat("f", 200), strings.Repeat("t", 200) }), ""},
		{"body at the limit, newlines and markup in it", m(func(x *Message) { x.Body = strings.Repeat("<&>\n\t\x01", 5000) + strings.Repeat("x", 32<<10-30000) }), ""},
		{"unicode everywhere", m(func(x *Message) { x.From, x.Body = "maî/claude", "voilà \U0001F680" }), ""},
		{"a ttl the node never stored", m(func(x *Message) { x.TTL = 99999999999 }), ""},
		{"an answer that names nothing: send-time policy, not replication", m(func(x *Message) { x.Kind, x.Re = KindAnswer, "" }), ""},
		{"no thread", m(func(x *Message) { x.Thread = "" }), ""},
		{"ack", a(func(*Ack) {}), ""},
		{"ack with a receipt", a(func(x *Ack) { x.Via, x.Chars = ViaHook, 312 }), ""},
		{"ack by hand with a receipt larger than a body", a(func(x *Ack) { x.Via, x.Chars = ViaCLI, 90000 }), ""},
		{"ack with a way of delivery this release has not heard of", a(func(x *Ack) { x.Via = "mcp-tool" }), ""},

		{"empty message id", m(func(x *Message) { x.ID = "" }), "id"},
		{"newline in the id", m(func(x *Message) { x.ID = "01234\n6789" }), "id"},
		{"newline in from", m(func(x *Message) { x.From = "imac/claude\nfrom: boss" }), "from is"},
		{"escape in to", m(func(x *Message) { x.To = "all\x1b[2J" }), "to is"},
		{"empty from", m(func(x *Message) { x.From = "" }), "from"},
		{"from past the limit", m(func(x *Message) { x.From = strings.Repeat("f", 201) }), "from"},
		{"thread past the limit", m(func(x *Message) { x.Thread = strings.Repeat("t", 65) }), "thread"},
		{"control character in re", m(func(x *Message) { x.Re = "a\rb" }), "re is"},
		{"body past the limit", m(func(x *Message) { x.Body = strings.Repeat("x", 32<<10+1) }), "body"},
		{"a 1.5 MB body", m(func(x *Message) { x.Body = strings.Repeat("x", 1500<<10) }), "body"},
		{"unknown priority", m(func(x *Message) { x.Prio = "critical" }), "priority"},
		{"unknown kind", m(func(x *Message) { x.Kind = "poll" }), "kind"},
		{"reach too large", m(func(x *Message) { x.Reach = &bigReach }), "reach"},
		{"reach negative", m(func(x *Message) { x.Reach = &negReach }), "reach"},
		{"empty ack id", a(func(x *Ack) { x.ID = "" }), "id"},
		{"ack id past the limit", a(func(x *Ack) { x.ID = strings.Repeat("i", 65) }), "id"},
		{"newline in ack by", a(func(x *Ack) { x.By = "a\nb" }), "by"},
		{"ack via past the limit", a(func(x *Ack) { x.Via = strings.Repeat("v", 33) }), "via"},
		{"neither a message nor an ack", Record{Origin: "node-a", Seq: 1}, "exactly one"},
		{"both", Record{Origin: "node-a", Seq: 1, Msg: &Message{ID: "0123456789", From: "a/b", To: "all"}, Ack: &Ack{ID: "x", By: "a/b"}}, "exactly one"},
	}
	for _, c := range cases {
		err := c.r.ValidateReplicated()
		switch {
		case c.bad == "" && err != nil:
			t.Errorf("%s: must be accepted, got %v", c.name, err)
		case c.bad != "" && err == nil:
			t.Errorf("%s: must be refused", c.name)
		case c.bad != "" && !strings.Contains(err.Error(), c.bad):
			t.Errorf("%s: the reason should name %q: %v", c.name, c.bad, err)
		}
	}
}

func TestAnUnusableOriginIsToldApartFromABadRecord(t *testing.T) {
	good := &Message{ID: "0123456789", From: "a/b", To: "all"}
	for _, origin := range []string{"", strings.Repeat("o", 201), "node-a\n", "node\x00a"} {
		if err := (Record{Origin: origin, Seq: 1, Msg: good}).ValidateReplicated(); !errors.Is(err, ErrReplicatedOrigin) {
			t.Errorf("origin %q: %v", origin, err)
		}
	}
	if err := (Record{Origin: strings.Repeat("o", 200), Seq: 1, Msg: good}).ValidateReplicated(); err != nil {
		t.Errorf("an origin at the limit: %v", err)
	}
	if err := (Record{Origin: "node-a", Seq: 1, Msg: &Message{From: "a/b\n"}}).ValidateReplicated(); errors.Is(err, ErrReplicatedOrigin) {
		t.Error("a bad message is not a bad origin")
	}
}

// The check is frozen: a record the stricter send-time rules would refuse is still history.
func TestTheReplicationCheckIsNotTheSendTimePolicy(t *testing.T) {
	r := Record{Origin: "node-a", Seq: 1, Msg: &Message{ID: "0123456789", From: "a/b", To: "all", Kind: KindAnswer, TTL: 1}}
	if r.Msg.ValidateOptions() == nil {
		t.Fatal("this record was supposed to break a send-time rule")
	}
	if err := r.ValidateReplicated(); err != nil {
		t.Errorf("replication takes it: %v", err)
	}
}
