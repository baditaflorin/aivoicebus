package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestLeaseRetentionOnNoticeCollisionsReservationsAndColdRepliesMatchOff(t *testing.T) {
	for _, before := range []bool{false, true} {
		var golden []msg.Message
		for _, mode := range []LeaseRetentionMode{LeaseRetentionOff, LeaseRetentionObserve, LeaseRetentionOn} {
			now := onViewsNow
			m := projectionLease(1, now, now.Add(time.Hour))
			reader := "reader/session"
			a := msg.Ack{ID: m.ID, By: reader, At: now, Via: msg.ViaInbox}
			retract := msg.Ack{ID: msg.RetractLeakPrefix + m.ID, By: m.From, At: now}
			collision := msg.Message{ID: msg.NoticeID(m.ID, 0), Thread: msg.NoticeID(m.ID, 0), From: "peer/foreign", To: "other/session", At: now, Body: "ordinary peer-chosen ID must survive"}
			records := []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}, {Origin: "peer", Seq: 2, Ack: &a}}
			if before {
				records = append(records, msg.Record{Origin: "peer", Seq: 3, Msg: &collision}, msg.Record{Origin: "peer", Seq: 4, Ack: &retract})
			} else {
				records = append(records, msg.Record{Origin: "peer", Seq: 3, Ack: &retract}, msg.Record{Origin: "peer", Seq: 4, Msg: &collision})
			}
			s := onStateStore(t, mode, records, &now)
			got := s.Inbox(reader, false)
			if len(got) != 1 || got[0].ID != msg.NoticeID(m.ID, 1) {
				t.Fatal("managed notice probe changed", before, mode, got)
			}
			if mode == LeaseRetentionOff {
				golden = got
			} else if !reflect.DeepEqual(got, golden) {
				t.Fatal("notice collision parity", mode, got, golden)
			}
			_, err := s.Send(msg.Message{ID: got[0].ID, From: "sender/session", To: reader, Body: "must refuse reserved notice id"})
			if !errors.Is(err, ErrReservedID) {
				t.Fatal("notice reservation lost", mode, err)
			}
			// Another managed root can use a peer-selected reserved ID. It is real
			// history, so replying to it must not mistake the cold root for a notice.
			real := projectionLease(2, now, now.Add(time.Hour))
			real.ID = got[0].ID
			real.Thread = real.ID
			if _, err := s.Merge([]msg.Record{{Origin: "foreign", Seq: 1, Msg: &real}}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Send(msg.Message{ID: "000000cc01", From: "reader/other", To: real.From, Re: real.ID, Body: "reply to held cold root"}); err != nil {
				t.Fatal("real cold root mistaken for notice", mode, err)
			}
		}
	}
}
