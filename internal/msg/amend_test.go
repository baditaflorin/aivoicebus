package msg

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAmendedIDReadsOnlyAWellFormedClaim(t *testing.T) {
	for in, want := range map[string]string{
		"amend:3f2a9c01de":       "3f2a9c01de",
		"amend:3F2A9C01DE":       "", // a claim names the message by the exact text
		"amend:3f2a9c01d":        "",
		"amend:3f2a9c01de0":      "",
		"amend:":                 "",
		"Amend:3f2a9c01de":       "", // the prefix is exact
		"amend:amend:3f2a9c01de": "", // nothing claims a claim
		"retract:3f2a9c01de":     "", // a retraction is another thing
		"3f2a9c01de":             "",
		"amend:3f2a9c01d\n":      "",
	} {
		got, ok := AmendedID(in)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	if !HasAmendPrefix("amend:whatever") || HasAmendPrefix("retract:3f2a9c01de") || HasAmendPrefix("Amend:x") {
		t.Error("HasAmendPrefix")
	}
}

func TestAClaimIsAnOrdinaryAckEveryReleaseKeeps(t *testing.T) {
	r := Record{Origin: "n1", Seq: 3, Ack: &Ack{ID: AmendPrefix + "3f2a9c01de", By: "laptop/claude.a1b2", At: time.Now()}}
	if err := r.ValidateReplicated(); err != nil {
		t.Errorf("an older node must store it untouched: %v", err)
	}
	b, _ := json.Marshal(r)
	if !strings.Contains(string(b), `"ack":{"id":"amend:3f2a9c01de"`) {
		t.Errorf("%s", b)
	}
	if len(AmendPrefix+"3f2a9c01de") > replMaxRef {
		t.Error("longer than the replication check allows")
	}
}

func TestAmendmentPair(t *testing.T) {
	o := Message{ID: "0000000001", From: "imac/claude.a", To: "air/codex", Thread: "0000000001", Body: "noon"}
	a := Message{ID: "0000000002", From: "imac/claude.b", To: "air/codex", Thread: "0000000001", Re: o.ID, Body: "13:00"}
	if !AmendmentPair(a, o) {
		t.Fatal("another session of the same machine to the same target in the same thread")
	}
	for name, mut := range map[string]func(a, o *Message){
		"another machine":    func(a, o *Message) { a.From = "other/claude.a" },
		"another target":     func(a, o *Message) { a.To = "air/codex.r1" },
		"another thread":     func(a, o *Message) { a.Thread = "0000000009" },
		"does not answer it": func(a, o *Message) { a.Re = "0000000009" },
		"answers nothing":    func(a, o *Message) { a.Re = "" },
		"itself":             func(a, o *Message) { a.ID = o.ID },
		"a question":         func(a, o *Message) { a.Kind = KindQuestion },
		"a task":             func(a, o *Message) { o.Kind = KindTask },
		"a node's notice":    func(a, o *Message) { a.From, o.From = NoticeFrom, NoticeFrom },
		"no sender":          func(a, o *Message) { a.From = "" },
	} {
		x, y := a, o
		mut(&x, &y)
		if AmendmentPair(x, y) {
			t.Errorf("%s: must not be an amendment", name)
		}
	}
	if !MayAmend("imac/claude.zz", "imac/claude.a") || MayAmend("air/codex.1", "imac/claude.a") || MayAmend("", "imac/claude.a") {
		t.Error("MayAmend is MayRetract: the machine is the authority")
	}
}

func TestAmendTextsAreFixedTemplatesWithoutPeerWords(t *testing.T) {
	at := time.Date(2026, 10, 21, 4, 15, 59, 0, time.FixedZone("x", 3*3600))
	for got, want := range map[string]string{
		AmendTag(at):                  "[amended 01:15Z]",
		AmendedTag("3d4e5f6a7b", at):  "[amended: replaced by 3d4e5f6a7b at 01:15Z]",
		AmendNotice(at, "0a1b2c3d4e"): "[amended 01:15Z: this replaces message 0a1b2c3d4e, which you read; use this text, not that one]",
	} {
		if got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}

func TestAmendFieldsAreResponseOnlyAndOmittedWhenEmpty(t *testing.T) {
	b, _ := json.Marshal(Message{ID: "0000000001", From: "a/b", To: "c", Body: "x"})
	if strings.Contains(string(b), "amend") {
		t.Errorf("%s", b)
	}
	b, _ = json.Marshal(Message{ID: "0000000002", From: "a/b", To: "c", Body: "x", Amends: &Amends{Of: "0000000001", Read: "0000000001"}, AmendedBy: &Amended{By: "0000000003", At: time.Unix(0, 0).UTC()}})
	for _, want := range []string{`"amends":{"of":"0000000001","read":"0000000001"}`, `"amended_by":{"by":"0000000003","at":"1970-01-01T00:00:00Z"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in %s", want, b)
		}
	}
}

func TestNodesAdvertiseAmend(t *testing.T) {
	found := false
	prev := ""
	for _, f := range NodeFeatures() {
		found = found || f == FeatureAmend
		if f < prev {
			t.Errorf("features are sorted: %q after %q", f, prev)
		}
		prev = f
	}
	if !found {
		t.Error("a node that applies amendments says so")
	}
}
