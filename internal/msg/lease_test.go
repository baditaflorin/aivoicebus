package msg

import (
	"testing"
	"time"
)

func TestLeaseHoldersAndKeys(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	live := []Lease{
		{Key: "hub:server.go", By: "b/codex.2", ID: "b1", At: t0.Add(time.Second), Until: t0.Add(time.Hour)},
		{Key: "hub:server.go", By: "a/claude.1", ID: "a1", At: t0, Until: t0.Add(30 * time.Minute)},
		{Key: "hub:server.go", By: "a/claude.1", ID: "a2", At: t0.Add(20 * time.Minute), Until: t0.Add(50 * time.Minute)}, // a renewal
		{Key: "deploy:prod", By: "c/gemini.3", ID: "c1", At: t0, Until: t0.Add(time.Hour)},
	}
	hs := Holders(live)
	if len(hs) != 2 || hs[0].Key != "deploy:prod" || hs[1].By != "a/claude.1" || !hs[1].Until.Equal(t0.Add(50*time.Minute)) {
		t.Errorf("holders: %+v", hs)
	}
	for k, want := range map[string]bool{"hub:server.go": true, "repo/branch": true, "a b": false, "": false, "x\\ny": false, "x;rm": false} {
		if ValidLeaseKey(k) != want {
			t.Errorf("ValidLeaseKey(%q)", k)
		}
	}
	exp := t0.Add(time.Hour)
	if l, ok := LeaseOf(Message{Kind: KindLease, Body: "hub:x\nediting", From: "a/b", ID: "1", Exp: &exp}); !ok || l.Key != "hub:x" {
		t.Errorf("LeaseOf: %+v %v", l, ok)
	}
	if err := (Message{Kind: KindLease}).ValidateOptions(); err == nil {
		t.Error("a lease without a ttl accepted")
	}
}
