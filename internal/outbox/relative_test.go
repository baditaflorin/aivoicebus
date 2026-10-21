package outbox

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestLintRelativeTimesNamesTheClockTimeAndChangesNothing(t *testing.T) {
	now := time.Date(2026, 10, 21, 22, 48, 20, 0, time.UTC)
	for body, want := range map[string][]string{
		"the disk fills in about 11 h":             {`"in about 11 h" is 2026-10-22 09:48Z`},
		"deploying in 15 min, reply if mid-change": {`"in 15 min" is 23:03Z`},
		"I will look again within 2 hours.":        {`"within 2 hours" is 2026-10-22 00:48Z`},
		"rolls back In ~30m and again in 1.5h":     {`"In ~30m" is 23:18Z`, `"in 1.5h" is 2026-10-22 00:18Z`},
		"in 1 day we rotate it":                    {`"in 1 day" is 2026-10-22 22:48Z`},
	} {
		got := LintRelativeTimes(body, now)
		if got == "" {
			t.Errorf("%q: nothing said", body)
		}
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%q: missing %s in %s", body, w, got)
			}
		}
		if !strings.Contains(got, "Nothing was changed") || !strings.Contains(got, "(sent at 22:48Z)") {
			t.Errorf("%s", got)
		}
	}
	for _, body := range []string{
		"deploy at 04:15Z", "built in the 1990s", "the build takes 5 minutes", "in March", "in 400 days", "in 0 min", "within the hour", "",
	} {
		if got := LintRelativeTimes(body, now); got != "" {
			t.Errorf("%q: %s", body, got)
		}
	}
	long := LintRelativeTimes("in 1 h, in 2 h, in 3 h, in 4 h, in 5 h", now)
	if !strings.Contains(long, "and 2 more") || strings.Count(long, `" is `) != 3 {
		t.Errorf("a long list is cut: %s", long)
	}
}

func TestPrepareWarnsAboutARelativeTimeInAnAuthoredMessageButSendsItAsWritten(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	var warn bytes.Buffer
	p, err := Prepare(c, Request{To: "air/codex", Body: "deploying in 15 min, reply if mid-change", Authored: true, Queue: true}, &warn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), `"in 15 min" is `) || p.Msg.Body != "deploying in 15 min, reply if mid-change" {
		t.Errorf("%q / %q", warn.String(), p.Msg.Body)
	}
	warn.Reset()
	if _, err := Prepare(c, Request{To: "air/codex", Body: "worker: retrying in 15 min", Queue: true}, &warn); err != nil || strings.Contains(warn.String(), "in 15 min") {
		t.Errorf("a message the tool wrote itself is left alone: %v %q", err, warn.String())
	}
}

func TestAmendWarnsAboutARelativeTimeInTheCorrection(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	o := put(t, s, meID, "air/codex", "deploy at noon")
	_, out, err := amend(t, c, o.ID, "deploy in about 11 h", AmendOptions{})
	if err != nil || !strings.Contains(out, `warning: the text says "in about 11 h" is `) {
		t.Errorf("%v\n%s", err, out)
	}
}
