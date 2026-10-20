package hook

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// detail is a second line long enough that leaving it out saves more than the pointer costs.
const detail = "the free space is gone, builds fail, the run directories are full of node_modules, nothing else needs doing right now"

// fresh is the second line of a broadcast that must not be shortened; it shares nothing with detail.
const fresh = "see the changelog for what changed in this release, there is nothing you need to do about it"

// incident is a broadcast body: a one-line summary, then the detail nobody who was not involved needs.
func incident() string {
	return "VM120 disk emergency over: disk grown 256 -> 512 GB\n" + strings.Repeat("removed node_modules of the idle run dirs, details follow. ", 25) + "\nLAST-LINE-OF-THE-BODY"
}

// receipts is the characters each hook receipt says were injected, by message id.
func receipts(s *store.Store) map[string]int {
	out := map[string]int{}
	for _, r := range s.Since(map[string]uint64{}, 1000) {
		if r.Ack != nil && r.Ack.Via == msg.ViaHook {
			out[r.Ack.ID] = r.Ack.Chars
		}
	}
	return out
}

func TestSessionStartShowsALongBroadcastAsOneLineAndLeavesADirectMessageAlone(t *testing.T) {
	s, _ := mesh(t)
	bcast, _ := s.Send(msg.Message{From: "other/agent", To: "all", Body: incident()})
	direct, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "can you look at the build?\nit fails on step 3"})
	longDirect, _ := s.Send(msg.Message{From: "other/agent", To: "air/codex", Body: "LONGDIRECT " + strings.Repeat("x", 1500)})

	got := runHook(t, "")
	if !strings.Contains(got, "\nVM120 disk emergency over: disk grown 256 -> 512 GB [aivoicebus thread "+bcast.ID+" for the rest]\n") {
		t.Errorf("a long broadcast is its first line and the pointer:\n%s", got)
	}
	if strings.Contains(got, "removed node_modules") || strings.Contains(got, "LAST-LINE") {
		t.Errorf("the rest of a long broadcast must not be injected:\n%s", got)
	}
	if !strings.Contains(got, "\ncan you look at the build?\nit fails on step 3\n") {
		t.Errorf("a short direct message is untouched:\n%s", got)
	}
	if !strings.Contains(got, "LONGDIRECT "+strings.Repeat("x", maxBody-len("LONGDIRECT "))+" […cut; full text: aivoicebus thread "+longDirect.ID+"]") {
		t.Errorf("a long direct message keeps its 300-character cut:\n%s", got)
	}

	// the receipts say what each message really cost
	r := receipts(s)
	if r[bcast.ID] == 0 || r[bcast.ID] >= r[longDirect.ID] || r[direct.ID] == 0 {
		t.Errorf("receipts: broadcast %d, direct %d, long direct %d: the headline must cost less than the cut it replaced", r[bcast.ID], r[direct.ID], r[longDirect.ID])
	}
	var b strings.Builder
	view.Brief(&b, []msg.Message{bcast}, maxBody)
	if want := len([]rune(b.String())); r[bcast.ID] >= want {
		t.Errorf("the broadcast cost %d, no less than the %d it cost before", r[bcast.ID], want)
	}
}

func TestSessionStartShowsAnOldBroadcastAsOneLineButNotARecentOne(t *testing.T) {
	s, _ := mesh(t)
	s.SetClock(func() time.Time { return time.Now().Add(-7 * time.Hour) })
	oldID, _ := s.Send(msg.Message{From: "other/agent", To: "all", Body: "all-clear: scans fine again\n" + detail})
	s.SetClock(func() time.Time { return time.Now().Add(-5 * time.Hour) })
	s.Send(msg.Message{From: "other/agent", To: "*/codex", Body: "release 0.11.5 is out\n" + fresh})
	s.SetClock(time.Now)

	got := runHook(t, "")
	if !strings.Contains(got, "all-clear: scans fine again [aivoicebus thread "+oldID.ID+" for the rest]") || strings.Contains(got, "node_modules") {
		t.Errorf("a broadcast 7 hours old is a headline:\n%s", got)
	}
	if !strings.Contains(got, "\nrelease 0.11.5 is out\n"+fresh+"\n") {
		t.Errorf("a broadcast 5 hours old is shown whole:\n%s", got)
	}
}

func TestSessionStartDoesNotShortenAnUrgentBroadcast(t *testing.T) {
	s, _ := mesh(t)
	s.SetClock(func() time.Time { return time.Now().Add(-30 * time.Hour) })
	s.Send(msg.Message{From: "other/agent", To: "all", Body: incident(), Prio: msg.PrioUrgent})
	s.SetClock(time.Now)
	got := runHook(t, "")
	if strings.Contains(got, "for the rest") || !strings.Contains(got, "removed node_modules") || !strings.Contains(got, "…cut; full text: aivoicebus thread") {
		t.Errorf("an urgent broadcast keeps the usual cut:\n%s", got)
	}
}

func TestSessionStartShowsTheOpenerOfAFinishedThreadAsOneLine(t *testing.T) {
	s, _ := mesh(t)
	open, _ := s.Send(msg.Message{From: "other/agent", To: "all", Body: "VM120 disk 99%: stop writing\n" + detail})
	s.Send(msg.Message{From: "other/agent", To: "all", Kind: msg.KindEnd, Re: open.ID, Thread: open.ID, Body: "fixed, carry on"})
	got := runHook(t, "")
	if !strings.Contains(got, "VM120 disk 99%: stop writing [aivoicebus thread "+open.ID+" for the rest]") || strings.Contains(got, "node_modules") {
		t.Errorf("the opener of a thread that its own batch ends is a headline:\n%s", got)
	}
	if !strings.Contains(got, "fixed, carry on") {
		t.Errorf("the closing message is shown:\n%s", got)
	}
}

func TestThePromptHookNeverShortensWhatItShows(t *testing.T) {
	s, _ := mesh(t)
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	s.Send(msg.Message{From: "other/agent", To: "all", Body: incident(), Prio: msg.PrioUrgent})
	var out strings.Builder
	Run([]string{"--tool", "claude", "--event", "prompt"}, nil, &out)
	if got := out.String(); strings.Contains(got, "for the rest") || !strings.Contains(got, "URGENT message(s) arrived") || !strings.Contains(got, "removed node_modules") {
		t.Errorf("an urgent broadcast is shown with the usual cut on a prompt:\n%s", got)
	}
}
