package outbox

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestPickReplyInfersOnlyWhenExactlyOneMessageCouldBeAnswered(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return now.Add(-time.Duration(m) * time.Minute) }
	retracted := &msg.Retraction{}
	gone := at(1)
	cases := []struct {
		name string
		in   []msg.Message
		want string // id of the one candidate; "" with kind says why not
		kind error
	}{
		{"one", []msg.Message{{ID: "a", At: at(30)}}, "a", nil},
		{"a node's notice is not a candidate", []msg.Message{{ID: "a", At: at(30)}, {ID: "n", From: msg.NoticeFrom, At: at(1)}}, "a", nil},
		{"a withdrawn message is not a candidate", []msg.Message{{ID: "a", At: at(30)}, {ID: "w", At: at(1), Retracted: retracted}}, "a", nil},
		{"an expired one is not a candidate", []msg.Message{{ID: "a", At: at(30)}, {ID: "e", At: at(1), Exp: &gone}}, "a", nil},
		{"two: the newest is no safe guess", []msg.Message{{ID: "a", At: at(30)}, {ID: "b", At: at(5), From: "evil/x"}}, "", ErrUsage},
		{"three", []msg.Message{{ID: "a"}, {ID: "b"}, {ID: "c"}}, "", ErrUsage},
		{"nothing", nil, "", ErrNotFound},
		{"only notices", []msg.Message{{ID: "n", From: msg.NoticeFrom}}, "", ErrNotFound},
	}
	for _, c := range cases {
		m, err := pickReply(c.in, now)
		if c.kind == nil {
			if err != nil || m.ID != c.want {
				t.Errorf("%s: picked %q, err %v; want %q", c.name, m.ID, err, c.want)
			}
			continue
		}
		if !errors.Is(err, c.kind) || m.ID != "" {
			t.Errorf("%s: want kind %v, got %q %v", c.name, c.kind, m.ID, err)
		}
	}
	// The refusal names every candidate with its sender and says how to choose.
	_, err := pickReply([]msg.Message{{ID: "aaaaaaaaaa", From: "x/one"}, {ID: "bbbbbbbbbb", From: "y/two"}}, now)
	for _, want := range []string{"aaaaaaaaaa (from x/one)", "bbbbbbbbbb (from y/two)", "--re ID"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q: %v", want, err)
		}
	}
}

func TestLastUnreadReadsTheInboxAndSkipsWhatWasRead(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	first := putID(t, s, "1111111111", elsewho, meID, "first")
	second := putID(t, s, "2222222222", reader1, meID, "second")
	if _, err := LastUnread(c, meID); !errors.Is(err, ErrUsage) {
		t.Fatalf("two unread: nothing is guessed: %v", err)
	}
	if err := c.Ack(second.ID, meID); err != nil {
		t.Fatal(err)
	}
	if m, err := LastUnread(c, meID); err != nil || m.ID != first.ID {
		t.Errorf("one left: %+v %v", m, err)
	}
	if err := c.Ack(first.ID, meID); err != nil {
		t.Fatal(err)
	}
	if _, err := LastUnread(c, meID); !errors.Is(err, ErrNotFound) {
		t.Errorf("none left: %v", err)
	}
}
