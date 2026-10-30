package msg

import (
	"strings"
	"testing"
)

func TestSettleIDsAndWhoMaySettle(t *testing.T) {
	if id, ok := SettledID(SettlePrefix + "a1b2c3d4e5"); !ok || id != "a1b2c3d4e5" {
		t.Errorf("SettledID: %q %v", id, ok)
	}
	for _, bad := range []string{"settle:", "settle:xyz", "settle:A1B2C3D4E5", "settle:a1b2c3d4e5f", "amend:a1b2c3d4e5", "a1b2c3d4e5"} {
		if _, ok := SettledID(bad); ok {
			t.Errorf("%q is no settle", bad)
		}
	}
	if !HasSettlePrefix("settle:junk") || HasSettlePrefix("amend:a1b2c3d4e5") {
		t.Error("HasSettlePrefix")
	}
	if !MaySettle("air/human", "air") || !MaySettle("AIR/human", "air") {
		t.Error("the machine's person may settle for it")
	}
	if MaySettle("air/claude.a1", "air") || MaySettle("mac/human", "air") || MaySettle("air", "air") {
		t.Error("only <machine>/human, for its own machine")
	}
}

// What must be answered is never settled, whoever asks; the mesh's own messages neither.
func TestWhatMustBeAnsweredIsNeverSettleable(t *testing.T) {
	for _, body := range []string{TokenHandTag + " air\nx", ApprovalTag + " run it?", AskPrefix + "which one?"} {
		m := Message{ID: "a1b2c3d4e5", From: "desk/claude", To: "air/human", Kind: KindTask, Body: body}
		if !MustAnswer(m) || Settleable(m) {
			t.Errorf("%q must be answered", body)
		}
	}
	for _, body := range []string{"hello", PersonTag + " update it", StuckPrefix + "blocked", "see " + ApprovalTag} {
		if m := (Message{ID: "a1b2c3d4e5", From: "desk/claude", To: "air", Body: body}); MustAnswer(m) || !Settleable(m) {
			t.Errorf("%q may be settled", body)
		}
	}
	if Settleable(Message{ID: "a1b2c3d4e5", From: "aivoicebus/notice", To: "air", Body: "x"}) {
		t.Error("a node's own message is not settled")
	}
}

func TestDoneBodyIsOneShortLine(t *testing.T) {
	if got := DoneBody(DoneByPersonTag, "  did it\nby hand  "); got != "[done by the person] did it by hand" {
		t.Errorf("%q", got)
	}
	if got := DoneBody(DoneByPersonTag, ""); got != DoneByPersonTag {
		t.Errorf("%q", got)
	}
	if got := DoneBody(DoneByPersonTag, strings.Repeat("é", 500)); len([]rune(got)) != len([]rune(DoneByPersonTag))+1+200 {
		t.Errorf("a reason is cut to 200 characters: %d", len([]rune(got)))
	}
}
