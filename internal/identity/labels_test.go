package identity

import (
	"strings"
	"testing"
)

// fakeGit makes the checkout containing every directory look like repo on branch
// ("" is a detached HEAD, repo "" is no checkout at all).
func fakeGit(t *testing.T, repo, branch string) {
	t.Helper()
	old := gitOutput
	t.Cleanup(func() { gitOutput = old })
	gitOutput = func(cwd string, args ...string) string {
		if repo == "" {
			return ""
		}
		switch args[0] {
		case "rev-parse":
			return "/work/" + repo
		case "config":
			return "git@github.com:owner/" + repo + ".git"
		case "symbolic-ref":
			return branch
		}
		return ""
	}
}

func labelsFor(t *testing.T, pinned string) map[string]string {
	t.Helper()
	t.Setenv("AIVOICEBUS_ME", "mac/claude.a1b2") // no hardware probing
	t.Setenv("AIVOICEBUS_LABELS", pinned)
	return Labels("/anywhere")
}

func TestLabelsComeFromTheCheckoutWhenNothingIsPinned(t *testing.T) {
	fakeGit(t, "alpha", "feat/x")
	l := labelsFor(t, "")
	if l["repo"] != "alpha" || l["branch"] != "feat/x" || l["tool"] != "claude" || l["machine"] != "mac" {
		t.Fatalf("%v", l)
	}
	if l["origin"] != "owner/alpha" {
		t.Errorf("a remote with an owner gives an origin label, to tell repos of one name apart: %v", l)
	}
}

func TestABranchSwitchedMidSessionInTheSameRepoIsFollowed(t *testing.T) {
	fakeGit(t, "alpha", "feat/y")
	l := labelsFor(t, "repo=alpha,branch=main,team=blue")
	if l["branch"] != "feat/y" || l["repo"] != "alpha" || l["team"] != "blue" {
		t.Fatalf("the session moved to feat/y in the repo it started in, keeping its other labels: %v", l)
	}
	if got := Labels("/anywhere").String(); !strings.Contains(got, "branch=feat/y") {
		t.Errorf("rendered labels: %s", got)
	}
}

func TestACommandRunFromAnotherRepoNeverRewritesTheSessionsLabels(t *testing.T) {
	fakeGit(t, "other", "feat/q")
	l := labelsFor(t, "repo=alpha,branch=main")
	if l["repo"] != "alpha" || l["branch"] != "main" {
		t.Fatalf("pinned repo and branch must stay: %v", l)
	}
}

func TestOutsideAnyCheckoutThePinnedLabelsStand(t *testing.T) {
	fakeGit(t, "", "")
	l := labelsFor(t, "repo=alpha,branch=main")
	if l["repo"] != "alpha" || l["branch"] != "main" {
		t.Fatalf("%v", l)
	}
}

func TestADetachedHeadKeepsThePinnedBranch(t *testing.T) { // mid-rebase, say
	fakeGit(t, "alpha", "")
	l := labelsFor(t, "repo=alpha,branch=main")
	if l["branch"] != "main" {
		t.Fatalf("%v", l)
	}
}

func TestDeclaredCapabilitiesBecomeOneLabelAndNothingIsGuessed(t *testing.T) {
	fakeGit(t, "", "")
	t.Setenv("AIVOICEBUS_CAPS", "")
	if _, ok := labelsFor(t, "")["caps"]; ok {
		t.Fatal("no declaration, no caps label")
	}
	t.Setenv("AIVOICEBUS_CAPS", "run, ask,run")
	if got := labelsFor(t, "")["caps"]; got != "ask+run" {
		t.Fatalf("caps %q", got)
	}
}

func TestCapsAreReadAfreshNotFrozenByThePinnedLabels(t *testing.T) {
	fakeGit(t, "", "")
	t.Setenv("AIVOICEBUS_CAPS", "ask,run")
	pinned := "repo=alpha," + "caps=" + labelsFor(t, "")["caps"] // what an older hook would have exported
	t.Setenv("AIVOICEBUS_CAPS", "read-only")
	if got := labelsFor(t, pinned)["caps"]; got != "read-only" {
		t.Fatalf("dropping to read-only must take effect at once, got %q", got)
	}
	t.Setenv("AIVOICEBUS_CAPS", "")
	if got := labelsFor(t, "repo=alpha,caps=run")["caps"]; got != "run" {
		t.Errorf("with nothing declared in the environment an explicit pin stands: %q", got)
	}
}

func TestAFullLabelSetWarnsOnceThatCapsAreNotAdvertised(t *testing.T) {
	fakeGit(t, "", "")
	var warn strings.Builder
	old, oldW := warnOut, capsDropWarned.Load()
	warnOut = &warn
	capsDropWarned.Store(false)
	t.Cleanup(func() { warnOut = old; capsDropWarned.Store(oldW) })
	t.Setenv("AIVOICEBUS_CAPS", "run")
	var pairs []string
	for _, k := range []string{"aa", "ab", "ac", "ad", "ae", "af", "ag", "ah", "ai", "aj", "ak", "al"} {
		pairs = append(pairs, k+"=1")
	}
	full := strings.Join(pairs, ",")
	if _, ok := labelsFor(t, full)["caps"]; ok {
		t.Fatal("the set was meant to be full")
	}
	labelsFor(t, full)
	if got := warn.String(); strings.Count(got, "AIVOICEBUS_CAPS is not advertised") != 1 {
		t.Errorf("one warning, not one per call: %q", got)
	}
	warn.Reset()
	capsDropWarned.Store(false)
	labelsFor(t, "")
	if warn.Len() != 0 {
		t.Errorf("no warning when caps fit: %q", warn.String())
	}
}
