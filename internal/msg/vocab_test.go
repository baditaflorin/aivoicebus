package msg

import (
	"os"
	"strings"
	"testing"
)

// The words of the tables are what every checker accepts: the send-time policy, the frozen
// replication check (which a release may only widen) and the constants themselves.
func TestTheVocabularyTablesAreWhatEveryCheckAccepts(t *testing.T) {
	if got := strings.Join(PrioNames(), ","); got != "fyi,ask,urgent" {
		t.Errorf("priorities %q: the order is part of the help text and the schemas", got)
	}
	if got := strings.Join(KindNames(), ","); got != "question,answer,task,claim,end" {
		t.Errorf("kinds %q", got)
	}
	for _, p := range append([]string{""}, PrioNames()...) {
		m := Message{Prio: p}
		if err := m.ValidateOptions(); err != nil {
			t.Errorf("prio %q: %v", p, err)
		}
		if err := m.validateReplicated(); err != nil && !strings.Contains(err.Error(), "id") {
			t.Errorf("prio %q refused on replication: %v", p, err)
		}
	}
	for _, k := range append([]string{""}, KindNames()...) {
		m := Message{Kind: k, Re: "89abcdef01"}
		if err := m.ValidateOptions(); err != nil {
			t.Errorf("kind %q: %v", k, err)
		}
	}
	for _, bad := range []string{"FYI", "high", " ask", "ask "} {
		m := Message{Prio: bad}
		if m.ValidateOptions() == nil {
			t.Errorf("prio %q accepted", bad)
		}
		ok := Message{ID: "0123456789", From: "a/b", To: "all", Thread: "0123456789", Prio: bad}
		if ok.validateReplicated() == nil {
			t.Errorf("replication takes prio %q: the frozen check and the table disagree", bad)
		}
		k := Message{Kind: bad}
		if k.ValidateOptions() == nil {
			t.Errorf("kind %q accepted", bad)
		}
	}
	if err := (Message{Prio: "loud"}).ValidateOptions(); err == nil || err.Error() != `priority "loud" is not one of fyi, ask, urgent` {
		t.Errorf("the refusal reads %v", err)
	}
	if err := (Message{Kind: "note"}).ValidateOptions(); err == nil || err.Error() != `kind "note" is not one of question, answer, task, claim, end` {
		t.Errorf("the refusal reads %v", err)
	}
	if (Message{}).Priority() != PrioAsk || Tag(Priorities, PrioAsk) != "" {
		t.Error("the default priority is ask and carries no tag")
	}
	for _, w := range append(append([]Word{}, Priorities...), Kinds...) {
		if w.Name == "" || (w.Tag != "" && w.Tag != "["+w.Name+"]") {
			t.Errorf("word %+v: a tag is [name] or nothing", w)
		}
	}
}

// The docs list the words by hand; this keeps them to the table. A new word fails here until the
// docs name it.
func TestTheDocsNameEveryPriorityAndKind(t *testing.T) {
	for _, f := range []string{"../../docs/reference.md", "../assets/usage.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(b)
		if !strings.Contains(doc, "--prio "+PrioChoices()) {
			t.Errorf("%s does not say `--prio %s`", f, PrioChoices())
		}
		for _, k := range KindNames() {
			if !strings.Contains(doc, "--kind "+k) {
				t.Errorf("%s does not mention `--kind %s`", f, k)
			}
		}
	}
}
