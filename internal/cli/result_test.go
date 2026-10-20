package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAWorkersAnswerIsReadIntoFields(t *testing.T) {
	w := workerOf("[worker on lap] exit 0 after 11s.\n57\n[write mode] committed on branch aivoicebus/task-abc of /src/api (not pushed):\nd82fde0")
	if w == nil || w.Host != "lap" || w.Exit == nil || *w.Exit != 0 || w.Took != "11s" || w.Output != "57" || w.Branch != "aivoicebus/task-abc" {
		t.Fatalf("%+v", w)
	}
	if w := workerOf("[worker on lap] not run: denied by desk."); w == nil || w.Exit != nil || w.Host != "lap" {
		t.Errorf("not run: %+v", w)
	}
	if w := workerOf("[worker on lap] exit 0 after 11s (14627 tokens in (14000 cached), 7 out, $0.0123).\n57"); w == nil || w.TokensIn != 14627 || w.TokensOut != 7 || w.CostUSD != 0.0123 || w.Output != "57" {
		t.Errorf("tokens: %+v", w)
	}
	if workerOf("hello") != nil {
		t.Error("not a worker's answer")
	}
}

// --wait-reply --json prints one object: answered (with the worker's fields) or not.
func TestWaitReplyJSONIsOneResultObject(t *testing.T) {
	workerSetup(t, "desk")
	sendTask(t, boss, "lap", "what is 6*7")
	if code, _, e := runWorker(t, "--exec", "/bin/cat"); code != 0 {
		t.Fatalf("worker: %s", e)
	}
	t.Setenv("AIVOICEBUS_ME", boss)
	code, out, e := run(t, "", "task", "lap", "what is 6*7", "--wait-reply", "10s", "--json")
	var r resultView
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || !r.Answered || !r.Ended || len(r.Answers) != 1 ||
		r.Answers[0].Worker == nil || r.Answers[0].Worker.Exit == nil || *r.Answers[0].Worker.Exit != 0 || r.Answers[0].Worker.Output != "what is 6*7" ||
		r.TakenBy != wk {
		t.Fatalf("answered: %d %v\n%s\n%s", code, err, out, e)
	}
	if r.Size == nil || r.Size.Messages < 3 || r.Size.Chars < len("what is 6*7") || r.Size.EstTokens != r.Size.Chars/4 {
		t.Errorf("the conversation's size so far: %+v\n%s", r.Size, out)
	}
	code, out, _ = run(t, "", "task", "box", "nobody here", "--wait-reply", "2s", "--json")
	r = resultView{}
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != exitTimeout || r.Answered || len(r.Answers) != 0 || !strings.HasPrefix(r.Thread, r.ID[:4]) {
		t.Fatalf("timeout: %d %v %s", code, err, out)
	}
}
