package msg

import (
	"strings"
	"testing"
)

func TestCleanCaps(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ask,run", "ask+run"},
		{"run, ask ,run", "ask+run"}, // sorted, deduplicated
		{"z,y,x,w,v,u,t,s", "s+t+u+v+w+x+y+z"},
		{"Read-Only", "read-only"},
		{"ask+run", "ask+run"},
		{"", ""},
		{" , ,", ""},
		{"a;b", "a-b"}, // anything unsafe becomes '-', as in any label value
	} {
		if got := CleanCaps(c.in); got != c.want {
			t.Errorf("CleanCaps(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	long := ""
	for i := 0; i < 30; i++ {
		long += "word" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ","
	}
	if got := CleanCaps(long); len(got) > MaxLabelValue || got == "" || got[len(got)-1] == '+' {
		t.Errorf("a long list is cut at a whole word within %d: %q", MaxLabelValue, got)
	}
}

func TestExpandCaps(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"@can=run", "@caps=run|run+*|*+run|*+run+*"},
		{"@repo=x,can=run", "@repo=x,caps=run|run+*|*+run|*+run+*"},
		{"@can=ask|run", "@caps=ask|ask+*|*+ask|*+ask+*|run|run+*|*+run|*+run+*"},
		{"@repo=x,can!=read-only", "@repo=x,caps!=read-only|read-only+*|*+read-only|*+read-only+*"},
		{"@CAN=run", "@caps=run|run+*|*+run|*+run+*"}, // the parser reads keys case-insensitively, so this does too
		{"@can=*", "@caps=*"},
		{"@repo=x,can!=*", "@repo=x,caps!=*"},
		{"@repo=x", "@repo=x"},
		{"@candy=1", "@candy=1"},
		{"all", "all"},
		{"host/agent", "host/agent"},
	} {
		got, err := ExpandCaps(c.in)
		if err != nil || got != c.want {
			t.Errorf("ExpandCaps(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if got != c.in { // whatever it produces must be a selector every node since 0.6.0 reads
			if _, err := ParseSelector(got); err != nil {
				t.Errorf("ExpandCaps(%q) = %q does not parse: %v", c.in, got, err)
			}
		}
	}
}

func TestExpandCapsErrorsQuoteWhatTheSenderTyped(t *testing.T) {
	for _, c := range []struct{ in, quoted string }{
		{"@repo=x,can= x", `"can= x"`},
		{"@can=", `"can="`},
		{"@can=ru*n", `"can=ru*n"`},
		{"@can=a+b", `"can=a+b"`},
		{"@can=a|b|c", `"can=a|b|c"`},
		{"@can=*|run", `"can=*|run"`},
	} {
		_, err := ExpandCaps(c.in)
		if err == nil || !strings.Contains(err.Error(), c.quoted) || strings.Contains(err.Error(), "caps=") {
			t.Errorf("ExpandCaps(%q): %v (want an error quoting %s, not the rewritten term)", c.in, err, c.quoted)
		}
	}
}

func TestACapabilityTermIsAWholeWordMatch(t *testing.T) {
	has := func(caps string) Labels { return Labels{"caps": caps} }
	for _, c := range []struct {
		to    string
		match []string
		skip  []string
	}{
		{"@can=run", []string{"run", "ask+run", "run+x", "a+run+z", "ask+run+write"},
			[]string{"dry-run", "no-run", "cannot-run", "runaway", "run-all", "ask+dry-run", "prerun+x"}},
		{"@can=ask", []string{"ask", "ask+run"}, []string{"task", "ask-me", "run+task"}},
		{"@can=read", []string{"read", "read+run"}, []string{"read-only", "ask+read-only"}},
		{"@can=read-only", []string{"read-only", "ask+read-only"}, []string{"read"}},
		{"@can=ask|run", []string{"ask", "run", "ask+run", "x+run"}, []string{"task", "dry-run"}},
		{"@can=*", []string{"x", "ask+run"}, nil},
	} {
		to, err := ExpandCaps(c.to)
		if err != nil {
			t.Fatal(err)
		}
		sel, err := ParseSelector(to)
		if err != nil {
			t.Fatal(err)
		}
		for _, caps := range c.match {
			if !sel.Match(has(caps)) {
				t.Errorf("%s should match caps %q", c.to, caps)
			}
		}
		for _, caps := range c.skip {
			if sel.Match(has(caps)) {
				t.Errorf("%s must not match caps %q", c.to, caps)
			}
		}
		if sel.Match(Labels{"repo": "x"}) {
			t.Errorf("%s must not match a session that advertises nothing", c.to)
		}
	}
}

func TestCanNotExcludesExactlyTheSessionsThatHaveTheWord(t *testing.T) {
	to, _ := ExpandCaps("@repo=x,can!=read-only")
	sel, err := ParseSelector(to)
	if err != nil {
		t.Fatal(err)
	}
	for caps, want := range map[string]bool{
		"read-only": false, "ask+read-only": false, "read-only+run": false, "a+read-only+z": false,
		"read": true, "ask+run": true, "no-read-only": true, "": true,
	} {
		l := Labels{"repo": "x"}
		if caps != "" {
			l["caps"] = caps
		}
		if sel.Match(l) != want {
			t.Errorf("can!=read-only on caps %q: got %v, want %v", caps, !want, want)
		}
	}
	any, _ := ExpandCaps("@repo=x,can!=*")
	sel, _ = ParseSelector(any)
	if sel.Match(Labels{"repo": "x", "caps": "ask"}) || !sel.Match(Labels{"repo": "x"}) {
		t.Error("can!=* is the sessions that advertise nothing")
	}
}
