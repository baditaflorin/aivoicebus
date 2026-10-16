package outbox

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func cands(ids ...string) []idCand {
	var out []idCand
	for _, id := range ids {
		out = append(out, idCand{id: id, from: "peer/" + id[:2]})
	}
	return out
}

func TestResolveAmongPicksTheOneIDThatStartsWithThePrefix(t *testing.T) {
	all := cands("3f2a9c01de", "3f2b000001", "a1b2c3d4e5", "a1b2c3d4ff")
	cases := []struct {
		name, prefix string
		want         string
		kind         error // nil: success
		in           []string
	}{
		{"unique", "a1b2c3d4e", "a1b2c3d4e5", nil, nil},
		{"unique short", "3f2a", "3f2a9c01de", nil, nil},
		{"ambiguous", "3f2", "", ErrAmbiguous, []string{"3f2a9c01de (from peer/3f)", "3f2b000001"}},
		{"ambiguous lists both", "a1b2", "", ErrAmbiguous, []string{"a1b2c3d4e5", "a1b2c3d4ff"}},
		{"none", "ffff", "", ErrNotFound, nil},
	}
	for _, c := range cases {
		got, err := resolveAmong(c.prefix, all)
		if c.kind == nil {
			if err != nil || got.id != c.want {
				t.Errorf("%s: %q, %v", c.name, got.id, err)
			}
			continue
		}
		if !errors.Is(err, c.kind) || got.id != "" {
			t.Errorf("%s: want kind %v, got %q, %v", c.name, c.kind, got.id, err)
			continue
		}
		for _, id := range c.in {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("%s: the error should list candidate %s: %v", c.name, id, err)
			}
		}
	}
	// Only the start of an id counts: the same digits in the middle of another do not make it ambiguous.
	if got, err := resolveAmong("3f2a", cands("3f2a9c01de", "003f2a9c01")); err != nil || got.id != "3f2a9c01de" {
		t.Errorf("a prefix is not a substring: %q %v", got.id, err)
	}
	// Many candidates: the list is capped and says how many more there are.
	var many []string
	for i := 0; i < 8; i++ {
		many = append(many, "ab0000000"+string(rune('0'+i)))
	}
	if _, err := resolveAmong("ab00", cands(many...)); !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "and 3 more") {
		t.Errorf("%v", err)
	}
}

func TestResolveFullIDNeedsNoNodeAndBadInputIsAUsageError(t *testing.T) {
	r := NewIDResolver(&client.Client{}, meID) // no node configured: any request would fail
	if got, err := r.Resolve(" 3F2A9C01DE "); err != nil || got != "3f2a9c01de" {
		t.Errorf("a full id is taken as typed (lower-cased): %q %v", got, err)
	}
	for _, bad := range []string{"", "xyz1", "3f2", "3f2a9c01de0", "not-an-id"} {
		if _, err := r.Resolve(bad); !errors.Is(err, ErrUsage) || errors.Is(err, ErrNotFound) {
			t.Errorf("%q is a usage error, not a search: %v", bad, err)
		}
	}
	if _, err := r.Resolve("3f2"); err == nil || !strings.Contains(err.Error(), "at least 4") {
		t.Errorf("a short prefix says how long it must be: %v", err)
	}
}

// putID stores a message with a chosen id, so a prefix of it is the same on every run.
func putID(t *testing.T, s *store.Store, id, from, to, body string) msg.Message {
	t.Helper()
	m, err := s.Send(msg.Message{ID: id, From: from, To: to, Body: body})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestResolveFindsInboxAndSentMessagesByPrefixAndSaysWhatItResolved(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	inbound := putID(t, s, "3f2a9c01de", elsewho, meID, "question for you")
	at(t, noon)
	rememberSent(msg.Message{ID: "9b8c7d6e5f", From: meID, To: reader1})

	var said bytes.Buffer
	r := NewIDResolver(c, meID)
	r.Say = &said
	if got, err := r.Resolve("3f2a9"); err != nil || got != inbound.ID {
		t.Errorf("an inbox message: %q %v", got, err)
	}
	if want := "resolved 3f2a9 -> 3f2a9c01de from " + elsewho + "\n"; said.String() != want {
		t.Errorf("a resolved prefix is shown with its sender: %q", said.String())
	}
	if got, err := r.Resolve("9B8C7D"); err != nil || got != "9b8c7d6e5f" {
		t.Errorf("a message this machine sent: %q %v", got, err)
	}
	if _, err := r.Resolve("ffff"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a prefix nobody has: %v", err)
	}
}

func TestResolveSentLooksOnlyAtWhatThisMachineSentAndNeedsSixCharacters(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	// A peer plants a message whose id starts like ours: the inbox must never decide a retraction.
	putID(t, s, "ab12cd0001", elsewho, meID, "planted")
	at(t, noon)
	rememberSent(msg.Message{ID: "ab12ef0002", From: meID, To: reader1})

	r := NewIDResolver(c, meID)
	var said bytes.Buffer
	r.Say = &said
	id, to, when, err := r.ResolveSent("ab12ef")
	if err != nil || id != "ab12ef0002" || to != reader1 || !when.Equal(noon) {
		t.Errorf("%q %q %v %v", id, to, when, err)
	}
	if !strings.Contains(said.String(), "resolved ab12ef -> ab12ef0002") {
		t.Errorf("%q", said.String())
	}
	if _, _, _, err := r.ResolveSent("ab12cd"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an id only the inbox holds is not found for retract: %v", err)
	}
	if _, _, _, err := r.ResolveSent("ab12e"); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "at least 6") {
		t.Errorf("five characters are too few to retract by: %v", err)
	}
	if id, to, _, err := r.ResolveSent("ab12ef0002"); err != nil || id != "ab12ef0002" || to != reader1 {
		t.Errorf("a full id is returned, with its remembered target when there is one: %q %q %v", id, to, err)
	}
	if got, err := r.ExpandSent("ab12ef0", 8); !errors.Is(err, ErrUsage) {
		t.Errorf("the MCP minimum of 8: %q %v", got, err)
	}
	if got, err := r.ExpandSent("ab12ef00", 8); err != nil || got != "ab12ef0002" {
		t.Errorf("%q %v", got, err)
	}
}

func TestResolveSaysSoWhenTheNodesCannotBeAsked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := &client.Client{URLs: []string{"http://127.0.0.1:1"}, Token: "tok", Timeout: time.Second}
	_, err := NewIDResolver(c, meID).Resolve("3f2a")
	if !client.IsUnreachable(err) || errors.Is(err, ErrNotFound) {
		t.Errorf("an unreachable mesh is not 'no such id': %v", err)
	}
}

func TestPrepareTakesAPrefixForReAndRepliesToItsSender(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	inbound := putID(t, s, "3f2a9c01de", elsewho, meID, "can you check the deploy?")
	var warn bytes.Buffer
	p, err := Prepare(c, Request{Re: "3f2a9", Body: "checked, fine"}, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if p.Msg.Re != inbound.ID || p.Msg.To != elsewho {
		t.Errorf("the reply carries the full id and goes back to the sender: %+v", p.Msg)
	}
	if !strings.Contains(warn.String(), "resolved 3f2a9 -> 3f2a9c01de from "+elsewho) {
		t.Errorf("the resolution is shown: %q", warn.String())
	}
	if _, err := Prepare(c, Request{Re: "ffff9", Body: "x"}, io.Discard); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "--re") {
		t.Errorf("a prefix that names nothing is an error, not a reply to nobody: %v", err)
	}
	if _, err := Prepare(c, Request{Re: "free-text-ref", To: reader1, Body: "x"}, io.Discard); err != nil {
		t.Errorf("a --re that is not hex is passed through as before: %v", err)
	}
	if _, err := Prepare(c, Request{To: reader1}, io.Discard); !errors.Is(err, ErrUsage) {
		t.Errorf("a send with no body is a usage error: %v", err)
	}
	if _, err := Prepare(c, Request{To: reader1, Body: "x", Prio: "loud"}, io.Discard); !errors.Is(err, ErrUsage) {
		t.Errorf("a bad --prio is a usage error: %v", err)
	}
}

func TestAThreadIsNeverResolvedByPrefix(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	putID(t, s, "ab12000001", elsewho, meID, "planted to look like a thread")
	putID(t, s, "cd34000009", elsewho, meID, "and one that a prefix would match alone")
	at(t, noon)
	rememberSent(msg.Message{ID: "ab12000002", From: meID, To: reader1})
	for _, th := range []string{"ab12", "ab12000001", "dead", "cd34"} {
		p, err := Prepare(c, Request{To: reader1, Thread: th, Body: "x"}, io.Discard)
		if err != nil || p.Msg.Thread != th {
			t.Errorf("--thread %s is used exactly as typed: %+v %v", th, p.Msg, err)
		}
	}
}
