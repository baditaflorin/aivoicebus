package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
)

func TestScheduleDueTimes(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 8, 0, 0, 0, time.Local)
	ev := schedule{Every: time.Hour, Added: t0}
	if ev.due(t0.Add(59*time.Minute)) || !ev.due(t0.Add(time.Hour)) {
		t.Error("--every counts from when it was added")
	}
	ev.Last = t0.Add(time.Hour)
	if ev.due(t0.Add(90*time.Minute)) || !ev.due(t0.Add(2*time.Hour)) {
		t.Error("then from when it was last sent")
	}
	at := schedule{At: "09:00", Added: t0}
	if at.due(t0.Add(59*time.Minute)) || !at.due(t0.Add(time.Hour)) {
		t.Error("--at fires at that time")
	}
	at.Last = t0.Add(time.Hour)
	if at.due(t0.Add(5*time.Hour)) || !at.due(t0.Add(25*time.Hour)) {
		t.Error("once a day")
	}
	late := schedule{At: "07:00", Added: t0}
	if late.due(t0.Add(time.Hour)) {
		t.Error("added after today's time: not until tomorrow")
	}
}

func TestScheduleAddListRunRemove(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", boss)
	for _, bad := range [][]string{
		{"schedule", "add", "x", "lap", "do it"},                                   // no --every / --at
		{"schedule", "add", "x", "--every", "1m", "lap", "do it"},                  // too often
		{"schedule", "add", "x y", "--every", "1h", "lap", "do it"},                // two words
		{"schedule", "add", "x", "--at", "25:00", "lap", "do it"},                  // no such time
		{"schedule", "add", "x", "--every", "1h", "--at", "09:00", "lap", "do it"}, // both
	} {
		if code, _, e := run(t, "", bad...); code != exitUsage {
			t.Errorf("%v: %d %s", bad, code, e)
		}
	}
	if code, out, e := run(t, "", "schedule", "add", "nightly", "--every", "6h", "lap", "--tool", "codex", "run the suite"); code != 0 || !strings.Contains(out, "lap/codex every 6h0m0s") {
		t.Fatalf("add: %d %s %s", code, out, e)
	}
	if _, out, _ := run(t, "", "schedule", "list"); !strings.Contains(out, "nightly") || !strings.Contains(out, "never yet") {
		t.Errorf("list: %s", out)
	}
	c, _ := client.FromEnv()
	var b strings.Builder
	if n, err := runSchedules(c, time.Now(), &b); n != 0 || err != nil {
		t.Fatalf("not due yet: %d %v", n, err)
	}
	if n, err := runSchedules(c, time.Now().Add(6*time.Hour), &b); n != 1 || err != nil || !strings.Contains(b.String(), "sent task") {
		t.Fatalf("due: %d %v %s", n, err, b.String())
	}
	t.Setenv("AIVOICEBUS_ME", "lap/codex.c9")
	if _, out, _ := run(t, "", "inbox"); !strings.Contains(out, "[scheduled nightly] run the suite") || !strings.Contains(out, "desk/scheduler") {
		t.Errorf("the task did not reach lap's codex: %s", out)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	if n, _ := runSchedules(c, time.Now().Add(7*time.Hour), &b); n != 0 {
		t.Error("sent again before the next 6h")
	}
	if code, _, _ := run(t, "", "schedule", "rm", "nightly"); code != 0 {
		t.Error("rm")
	}
	if code, _, _ := run(t, "", "schedule", "rm", "nightly"); code != exitNotFound {
		t.Error("rm twice")
	}
}
