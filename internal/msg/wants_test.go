package msg

import (
	"strings"
	"testing"
)

func TestParseCondAcceptsTheFourConditionsAndNothingElse(t *testing.T) {
	good := map[string]string{
		"release>=0.15.0":               "release>=0.15.0",
		" release>=1.0.12 ":             "release>=1.0.12",
		"has:signing-key":               "has:signing-key",
		"HAS:Fleet-Guard:0.3.*":         "has:fleet-guard:0.3.*",
		"file:~/.local/bin/aivoicebus":  "file:~/.local/bin/aivoicebus",
		"service:com.badita.agent-tail": "service:com.badita.agent-tail",
		"service:aivoicebus.service":    "service:aivoicebus.service",
	}
	for in, want := range good {
		c, err := ParseCond(in)
		if err != nil || c.String() != want {
			t.Errorf("%q: %v %v", in, c, err)
		}
	}
	for _, in := range []string{
		"", "release", "release>=", "release>=1.2", "release>=1.2.3-rc1", "release>1.2.3", "release<=1.2.3",
		"has:", "has:nothing", "has:ssh", "file:/etc/passwd", "file:~/", "file:~/../x", "file:~/a//b", "file:~", "file:$HOME/x",
		"service:", "service:-x", "service:a b", "service:a;b", "service:$(id)", "service:`id`",
		"exec:id", "sh:id", "http://x", "release>=1.2.3\nfile:~/x", strings.Repeat("a", 300),
	} {
		if c, err := ParseCond(in); err == nil {
			t.Errorf("%q is no condition, got %v", in, c)
		}
	}
}

func TestWantsRoundTripThroughTheBodyAndCostNothingElse(t *testing.T) {
	w := Wants{DoneWhen: "release>=0.15.0", Key: "update-0own", Supersedes: "3f2a9c01de"}
	body := AppendWants("Update aivoicebus on 0own.\n\nthen run aivoicebus version.\n", w)
	want := "Update aivoicebus on 0own.\n\nthen run aivoicebus version.\n[done-when: release>=0.15.0]\n[key: update-0own]\n[supersedes: 3f2a9c01de]"
	if body != want {
		t.Fatalf("%q", body)
	}
	text, got := SplitWants(body)
	if got != w || text != "Update aivoicebus on 0own.\n\nthen run aivoicebus version." {
		t.Errorf("%q %+v", text, got)
	}
	if WantsOf(Message{Body: body}) != w || !WantsOf(Message{Body: "plain"}).Empty() {
		t.Error("WantsOf")
	}
	if got := AppendWants("x", Wants{}); got != "x" {
		t.Errorf("no wants, no lines: %q", got)
	}
}

// A line that is not a well-formed want is text, and ends the trailer: nothing is run or guessed.
func TestAMalformedWantStaysText(t *testing.T) {
	for _, body := range []string{
		"do it\n[done-when: file:/etc/passwd]",
		"do it\n[done-when: $(touch /tmp/x)]",
		"do it\n[key: has space]",
		"do it\n[supersedes: ABC]",
		"do it\n[done-when: release>=0.15.0]\n[done-when: release>=0.16.0]\nand then",
		"[done-when: release>=0.15.0]", // a body that is only a want has no text to settle
	} {
		text, w := SplitWants(body)
		if body == "[done-when: release>=0.15.0]" {
			if !w.Empty() || text != body {
				t.Errorf("%q: %q %+v", body, text, w)
			}
			continue
		}
		if strings.HasPrefix(body, "do it\n[done-when: release>=0.15.0]\n[done-when") {
			if !w.Empty() || !strings.Contains(text, "[done-when: release>=0.16.0]") {
				t.Errorf("a want said twice ends the trailer: %q %+v", text, w)
			}
			continue
		}
		if !w.Empty() || text != body {
			t.Errorf("%q: %q %+v", body, text, w)
		}
	}
}

func TestWantsValidateInTheWordsOfTheFlags(t *testing.T) {
	for w, frag := range map[Wants]string{
		{DoneWhen: "exec:id"}: "--done-when",
		{Key: "a b"}:          "--key",
		{Supersedes: "short"}: "--supersedes",
		{DoneWhen: "has:deploy-token", Key: "k.1", Supersedes: "3f2a9c01de"}: "",
	} {
		err := w.Validate()
		if (frag == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), frag)) {
			t.Errorf("%+v: %v", w, err)
		}
	}
}
