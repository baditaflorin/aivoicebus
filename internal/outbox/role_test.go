package outbox

import (
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func writeRoles(t *testing.T, text string) {
	t.Helper()
	dir := os.Getenv("HOME") + "/.config/aivoicebus"
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(dir+"/roles", []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAnUnpinnedRoleNeedsTrustAndThenSaysSo(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{
		"build-a/claude.b1": "role=build,repo=alpha",
		"build-b/claude.r1": "role=review",
	})
	_, err := Prepare(c, Request{To: "@Build", Body: "ship it?"}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "aivoicebus role pin build 'build-a/claude.*'") || !strings.Contains(err.Error(), "--trust-role") {
		t.Fatalf("an unpinned role is explained, not guessed: %v", err)
	}
	p, err := Prepare(c, Request{To: "@Build", Body: "ship it?", TrustRole: true}, os.Stderr)
	if err != nil || p.Msg.To != "build-a/claude.b1" || p.Reach != 1 || p.Role == nil {
		t.Fatalf("%q %d %v", p.Msg.To, p.Reach, err)
	}
	if p.Role.Line() != "resolved @build -> build-a/claude.b1 (unverified)" || !strings.Contains(p.Alias, "not verified") {
		t.Errorf("who it resolved to, and that it is unverified, must be said: %q %q", p.Role.Line(), p.Alias)
	}
	var b strings.Builder
	p.Describe(&b)
	if !strings.Contains(b.String(), "to        build-a/claude.b1") || !strings.Contains(b.String(), "resolved  @build resolved to") {
		t.Errorf("a dry run shows it too:\n%s", b.String())
	}
	// the environment is the same consent as the flag
	t.Setenv("AIVOICEBUS_TRUST_ROLES", "1")
	if _, err := Prepare(c, Request{To: "@build", Body: "again"}, os.Stderr); err != nil {
		t.Errorf("AIVOICEBUS_TRUST_ROLES=1: %v", err)
	}
}

func TestAnUnpinnedRoleIsNeverUsedForUrgentOrAttachments(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{"build-a/claude.b1": "role=build"})
	t.Setenv("AIVOICEBUS_TRUST_ROLES", "1")
	for name, r := range map[string]Request{
		"urgent": {To: "@build", Body: "now", Prio: "urgent", TrustRole: true},
		"a file": {To: "@build", Body: "see this", File: true, TrustRole: true},
		"a link": {To: "@build", Body: "log", Attach: true, TrustRole: true},
		"URGENT": {To: "@build", Body: "now", Prio: "URGENT"},
	} {
		if _, err := Prepare(c, r, os.Stderr); err == nil || !strings.Contains(err.Error(), "never used for an urgent message or a file or log link") {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a pinned one carries them
	writeRoles(t, "build=build-*/claude.*\n")
	p, err := Prepare(c, Request{To: "@build", Body: "now", Prio: "urgent"}, os.Stderr)
	if err != nil || p.Msg.To != "build-a/claude.b1" || p.Role.Line() != "resolved @build -> build-a/claude.b1 (matches your pin pattern; identities are self-declared)" {
		t.Errorf("%v %+v", err, p.Role)
	}
}

func TestAnIdleRealHolderCannotBeReplacedByALiveAttacker(t *testing.T) {
	c, s := meshWith(t, "me/claude.0001", map[string]string{"build-a/claude.b1": "role=build"})
	s.InboxFor("evil/claude.ffff", false, msg.ParseLabels("role=build"))
	t.Setenv("AIVOICEBUS_TRUST_ROLES", "1")
	_, err := Prepare(c, Request{To: "@build", Body: "x"}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "2 sessions") || !strings.Contains(err.Error(), "build-a/claude.b1") || !strings.Contains(err.Error(), "evil/claude.ffff") {
		t.Errorf("both claimants are named, neither is chosen: %v", err)
	}
}

func TestARoleAliasNeverGuessesBetweenHolders(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{
		"build-a/claude.1111": "role=build",
		"build-b/claude.2222": "role=build+review",
	})
	writeRoles(t, "build=build-*/claude.*\n")
	_, err := Prepare(c, Request{To: "@build", Body: "x"}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "2 live sessions") || !strings.Contains(err.Error(), "build-a/claude.1111") || !strings.Contains(err.Error(), "build-b/claude.2222") {
		t.Errorf("both candidates are named: %v", err)
	}
	if _, err := Prepare(c, Request{To: "@lint", Body: "x", TrustRole: true}, os.Stderr); err == nil || !strings.Contains(err.Error(), "no session claims @lint") {
		t.Errorf("%v", err)
	}
}

func TestAPinnedRoleIgnoresAForgedClaim(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{
		"evil/claude.ffff": "role=build",
	})
	writeRoles(t, "build=build-*/claude.*\n")
	if _, err := Prepare(c, Request{To: "@build", Body: "x", TrustRole: true}, os.Stderr); err == nil || !strings.Contains(err.Error(), "no holder you pinned") || !strings.Contains(err.Error(), "evil/claude.ffff") {
		t.Errorf("the forged claim is ignored and reported, trust or not: %v", err)
	}
	// the real one appears: it is the one, and the pin is part of the explanation
	c2, _ := meshWith(t, "me/claude.0001", map[string]string{
		"evil/claude.ffff":  "role=build",
		"build-a/claude.b1": "role=build",
	})
	writeRoles(t, "build=build-*/claude.*\n")
	p, err := Prepare(c2, Request{To: "@build", Body: "x"}, os.Stderr)
	if err != nil || p.Msg.To != "build-a/claude.b1" || !strings.Contains(p.Alias, "pinned to build-*/claude.*") || strings.Contains(p.Alias, "not verified") {
		t.Errorf("%q %q %v", p.Msg.To, p.Alias, err)
	}
}

func TestAnUnusableRolesFileIsAnErrorNotNoPins(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{"build-a/claude.b1": "role=build"})
	dir := os.Getenv("HOME") + "/.config/aivoicebus"
	os.MkdirAll(dir, 0o755)
	target := dir + "/elsewhere"
	os.WriteFile(target, []byte("build=*/x\n"), 0o644)
	if err := os.Symlink(target, dir+"/roles"); err != nil {
		t.Skip(err)
	}
	if _, err := Prepare(c, Request{To: "@build", Body: "x", TrustRole: true}, os.Stderr); err == nil || !strings.Contains(err.Error(), "roles file is unusable") {
		t.Errorf("%v", err)
	}
}

func TestNamesThatPassForOthersAreRefusedBeforeSendingButRealOnesAreNot(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", nil)
	for _, to := range []string{"build-a/cl\u0430ude.b1", "all\u200b", "build-a\u2215claude", "build-a/c\u3164laude.b1"} {
		if _, err := Prepare(c, Request{To: to, Body: "x"}, os.Stderr); err == nil || !strings.Contains(err.Error(), "refused: the target") {
			t.Errorf("%q: %v", to, err)
		}
	}
	for _, me := range []string{"aivoice\u200bbus/x", "aivoicebus/retraction", "me/cl\u0430ude.1", "ᴀivoicebus/x", "me/claude.\u3164"} {
		t.Setenv("AIVOICEBUS_ME", me)
		_, err := Prepare(c, Request{To: "a/b", Body: "x"}, os.Stderr)
		if err == nil || !strings.Contains(err.Error(), "this session's identity") || !strings.Contains(err.Error(), "AIVOICEBUS_HOST") {
			t.Errorf("%q: %v", me, err)
		}
	}
	// a real hostname in another script sends, and its sender can be replied to
	for _, me := range []string{"сервер/claude.a1b2", "école/claude.a1b2"} {
		t.Setenv("AIVOICEBUS_ME", me)
		if _, err := Prepare(c, Request{To: "сервер/codex.q1w2", Body: "x"}, os.Stderr); err != nil {
			t.Errorf("%q: %v", me, err)
		}
	}
}

func TestWhoRoleListsHoldersAndTheOutcome(t *testing.T) {
	c, _ := meshWith(t, "me/claude.0001", map[string]string{"a/claude.1111": "role=build", "b/claude.2222": "role=build"})
	writeRoles(t, "build=a/claude.*,b/claude.*\n")
	var b strings.Builder
	WhoRole(&b, c, "build", "me/claude.0001")
	out := b.String()
	if !strings.Contains(out, "a/claude.1111") || !strings.Contains(out, "b/claude.2222") || !strings.Contains(out, "2 live sessions") {
		t.Errorf("%s", out)
	}
	if _, ok := msg.IsRoleAlias("@build"); !ok {
		t.Error("sanity")
	}
}
