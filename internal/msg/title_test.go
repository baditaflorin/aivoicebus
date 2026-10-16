package msg

import (
	"strings"
	"testing"
	"time"
)

func TestCleanTitle(t *testing.T) {
	long := strings.Repeat("word ", 30)
	for in, want := range map[string]string{
		"Fix the hub login redirect":     "Fix the hub login redirect",
		"  spaced   out\ttitle\n":        "spaced out title",
		"line one\nline two":             "line one line two",
		"zero​width‮override":            "zerowidthoverride", // invisible and bidi characters are dropped, not turned into spaces
		"esc\x1b[31mred\x1b[0m":          "esc [31mred [0m",   // controls become spaces; the rest is inert text
		"ㅤ⠀":                             "",                  // only blank letters: nothing readable
		"":                               "",
		"   ":                            "",
		"naïve café 日本語":                 "naïve café 日本語",
		"bad \xff\xfe bytes":             "bad bytes",
		"emoji 🚀 title":                  "emoji 🚀 title",
		strings.Repeat("é", MaxTitle+20): strings.Repeat("é", MaxTitle),
		long:                             strings.TrimSpace(long[:MaxTitle]),
	} {
		got := CleanTitle(in)
		if got != want {
			t.Errorf("CleanTitle(%q) = %q, want %q", in, got, want)
		}
		if CleanTitle(got) != got {
			t.Errorf("CleanTitle is not idempotent on %q", got)
		}
	}
	if n := len([]rune(CleanTitle(strings.Repeat("x", 1000)))); n != MaxTitle {
		t.Errorf("a long title is cut to %d characters, got %d", MaxTitle, n)
	}
}

func TestMergeRostersTakesTheTitleOfTheMostRecentReportThatHadOne(t *testing.T) {
	t0 := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	a := []RosterEntry{{Me: "h/claude.a1b2", Seen: t0, Title: "old name"}}
	b := []RosterEntry{{Me: "h/claude.a1b2", Seen: t0.Add(time.Minute), Title: "new name"}}
	c := []RosterEntry{{Me: "h/claude.a1b2", Seen: t0.Add(2 * time.Minute)}} // a node that knows it only from a message
	got := MergeRosters(a, b, c)
	if len(got) != 1 || got[0].Title != "new name" || !got[0].Seen.Equal(t0.Add(2*time.Minute)) {
		t.Errorf("%+v", got)
	}
}

func TestReplicatedRecordsBoundTheTitleButNeverRefuseAnOrdinaryOne(t *testing.T) {
	ok := Record{Origin: "n1", Seq: 1, Msg: &Message{ID: "0123456789", From: "h/a", To: "all", Thread: "0123456789", Body: "x", FromTitle: "Fix the login"}}
	if err := ok.ValidateReplicated(); err != nil {
		t.Errorf("an ordinary title: %v", err)
	}
	for name, title := range map[string]string{"too long": strings.Repeat("x", replMaxTitle+1), "not text": "bad \xff bytes"} {
		bad := ok
		m := *ok.Msg
		m.FromTitle = title
		bad.Msg = &m
		if err := bad.ValidateReplicated(); err == nil || strings.Contains(err.Error(), "xxxx") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
