package msg

import (
	"slices"
	"strings"
	"testing"
)

func TestCapsOfReadsTheLabelsSoNothingNeedsDeclaring(t *testing.T) {
	l := Labels{"repo": "go-common", "origin": "baditaflorin/go-common", "branch": "feature/x", "tool": "claude",
		ModelLabel: "claude-opus-5-5", RoleLabel: "build+review", CapsLabel: "gpu+metal+prod"}
	got := CapsOf("mac/claude.a1b2", l)
	want := []string{"gpu", "metal", "prod", "repo:baditaflorin/go-common", "branch:feature/x", "tool:claude",
		"model:claude-opus-5-5", "role:build", "role:review"}
	if !slices.Equal(got, want) {
		t.Errorf("CapsOf = %v\nwant     %v", got, want)
	}
	// No origin remote: the repository's name. No tool label (an old node): the identity's tool.
	got = CapsOf("air/codex.9f", Labels{"repo": "scratch"})
	if !slices.Equal(got, []string{"repo:scratch", "tool:codex"}) {
		t.Errorf("CapsOf without origin or tool label = %v", got)
	}
	if got := CapsOf("other/agent", nil); !slices.Equal(got, []string{"tool:agent"}) {
		t.Errorf("CapsOf with no labels = %v", got)
	}
}

func TestParseNeeds(t *testing.T) {
	got, err := ParseNeeds(" Repo:baditaflorin/go-common , gpu,gpu,branch:feature/*")
	if err != nil || !slices.Equal(got, []string{"repo:baditaflorin/go-common", "gpu", "branch:feature/*"}) {
		t.Fatalf("ParseNeeds = %v, %v", got, err)
	}
	for in, says := range map[string]string{
		"":                   "names no capability",
		" , ":                "names no capability",
		"colour:blue":        "not a capability kind",
		"repo:":              "may only use",
		"repo:a b":           "may only use",
		"two words":          "one word",
		"a,b,c,d,e,f,g,h,i,": "at most 8",
	} {
		if _, err := ParseNeeds(in); err == nil || !strings.Contains(err.Error(), says) {
			t.Errorf("ParseNeeds(%q) = %v, want an error saying %q", in, err, says)
		}
	}
}

func TestLacksMatchesEveryNeedOrSaysWhichAreMissing(t *testing.T) {
	l := Labels{"repo": "go-common", "origin": "baditaflorin/go-common", "branch": "feature/x", "tool": "claude", CapsLabel: "gpu+prod"}
	me := "mac/claude.a1b2"
	for _, c := range []struct {
		needs string
		lacks []string
	}{
		{"repo:baditaflorin/go-common", nil},
		{"repo:go-common", nil}, // the name alone matches too
		{"repo:baditaflorin/*", nil},
		{"repo:someone-else/go-common", []string{"repo:someone-else/go-common"}},
		{"repo:baditaflorin/go-common,gpu,prod", nil},
		{"branch:feature/*,tool:claude", nil},
		{"gpu,cuda,tool:codex", []string{"cuda", "tool:codex"}},
		{"pro", []string{"pro"}}, // whole words: prod is not pro
	} {
		needs, err := ParseNeeds(c.needs)
		if err != nil {
			t.Fatal(err)
		}
		if got := Lacks(me, l, needs); !slices.Equal(got, c.lacks) {
			t.Errorf("Lacks(%s) = %v, want %v", c.needs, got, c.lacks)
		}
	}
}
