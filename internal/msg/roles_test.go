package msg

import (
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

func holder(me, roles string, ago time.Duration) RosterEntry {
	l := Labels{}
	if roles != "" {
		l[RoleLabel] = roles
	}
	return RosterEntry{Me: me, Seen: t0.Add(-ago), Labels: l}
}

func TestIsRoleAlias(t *testing.T) {
	for to, want := range map[string]string{
		"@build": "build", "@review": "review", "@ci-1": "ci-1",
		"@here": "", "@+infra": "", "@repo=x": "", "@build,tool=claude": "", "@": "", "build": "", "all": "", "@a|b": "", "@Build": "", "@all": "",
	} {
		got, ok := IsRoleAlias(to)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", to, got, ok)
		}
	}
}

func TestRolesOfReadsTheLabelStrictly(t *testing.T) {
	got := RolesOf(Labels{RoleLabel: "build+review+build+here+a=b+x1+x2+x3"})
	if strings.Join(got, ",") != "build,review,x1,x2" {
		t.Errorf("%v", got)
	}
	if len(RolesOf(Labels{})) != 0 {
		t.Error("no label, no roles")
	}
}

func TestPinsAreStrict(t *testing.T) {
	for pin, ok := range map[string]bool{
		"build-*/claude.*": true, "laptop/codex.build1": true, "*uild/claude.*": true, "lap*/*": true,
		"*": false, "": false, "*/claude.*": false, "**/x": false, "/claude.*": false, "laptop": false, "laptop/": false,
		"a/b/c": false, "Laptop/claude.*": false, "laptop/cl​aude": false, "laptop/cl\x01": false,
	} {
		if err := ValidRolePin(pin); (err == nil) != ok {
			t.Errorf("%+q: %v", pin, err)
		}
	}
	pins := ParseRolePins("# roles\nbuild = build-*/claude.*, laptop/codex.build1  # mine\nreview=laptop/claude.*\nbad name=x\nhere=x/y\nempty=\nlax=*,*/x,laptop\nbuild=laptop/codex.build1\n")
	if len(pins) != 2 || len(pins["build"]) != 2 || pins["build"][1] != "laptop/codex.build1" || pins["review"][0] != "laptop/claude.*" || len(pins["lax"]) != 0 {
		t.Errorf("%v", pins)
	}
}

func TestAStarNeverCrossesTheSlash(t *testing.T) {
	for _, c := range []struct {
		pin, me string
		want    bool
	}{
		{"build-*/claude.*", "build-a/claude.b1", true},
		{"build-*/claude.*", "BUILD-A/Claude.B1", true}, // identities are compared in lower case
		{"build-*/claude.*", "build-a/evil/claude.b1", false},
		{"build-*/claude.*", "build-a/b/claude.b1", false},
		{"lap*/claude.*", "laptop/x/claude.b1", false},
		{"lap*/claude.*", "laptop", false},
		{"laptop/*", "laptop/claude.b1", true},
		{"laptop/*", "laptop/x/y", false},
		{"lap*/claude.*", "evil/laptop/claude.b1", false},
	} {
		if got := pinMatches(c.pin, c.me); got != c.want {
			t.Errorf("%s vs %s: %v", c.pin, c.me, got)
		}
	}
}

func TestResolveRole(t *testing.T) {
	const me = "laptop/claude.me01"
	pin := map[string][]string{"build": {"build-*/claude.*"}}
	trust := RoleOpts{Trust: true}
	cases := []struct {
		name   string
		roster []RosterEntry
		pins   map[string][]string
		o      RoleOpts
		target string
		errHas string // "" means it resolves
		outHas string // an ignored claim reported
	}{
		{"pinned: the one live holder", []RosterEntry{holder("build-a/claude.b1", "build", time.Minute), holder("x/claude.zz", "", 0)}, pin, RoleOpts{}, "build-a/claude.b1", "", ""},
		{"pinned: one of several roles", []RosterEntry{holder("build-a/claude.b1", "review+build", time.Minute)}, pin, RoleOpts{}, "build-a/claude.b1", "", ""},
		{"pinned: a pin ignores a forged claim", []RosterEntry{holder("evil/claude.ffff", "build", 0), holder("build-a/claude.b1", "build", time.Minute)}, pin, RoleOpts{}, "build-a/claude.b1", "", "evil/claude.ffff"},
		{"pinned: only forged claims resolve to nobody", []RosterEntry{holder("evil/claude.ffff", "build", 0)}, pin, RoleOpts{}, "", "no holder you pinned", "evil/claude.ffff"},
		{"pinned: two live holders is ambiguous, never a guess", []RosterEntry{holder("build-a/claude.1111", "build", time.Minute), holder("build-b/claude.2222", "build", time.Hour)}, pin, RoleOpts{}, "", "2 live sessions", ""},
		{"pinned: a stale holder is no holder", []RosterEntry{holder("build-a/claude.1111", "build", 3*time.Hour)}, pin, RoleOpts{}, "", "no live holder", ""},
		{"pinned: a live holder beats a stale one", []RosterEntry{holder("build-a/claude.1111", "build", 5*time.Hour), holder("build-b/claude.2222", "build", time.Minute)}, pin, RoleOpts{}, "build-b/claude.2222", "", ""},
		{"pinned: a sighting from the future is not believed", []RosterEntry{holder("build-a/claude.1111", "build", -time.Hour)}, pin, RoleOpts{}, "", "no live holder", ""},
		{"pinned: matching is case-insensitive on both sides, and the target is the identity as the node holds it", []RosterEntry{holder("BUILD-A/Claude.B1", "build", time.Minute)}, pin, RoleOpts{}, "BUILD-A/Claude.B1", "", ""},
		{"pinned: a star does not cross the slash", []RosterEntry{holder("build-a/x/claude.b1", "build", time.Minute)}, pin, RoleOpts{}, "", "no holder you pinned", "build-a/x/claude.b1"},
		{"pinned: never itself", []RosterEntry{holder(me, "build", 0), holder("build-a/claude.1111", "build", time.Minute)}, map[string][]string{"build": {"*ild-a/*", "laptop/*"}}, RoleOpts{}, "build-a/claude.1111", "", ""},

		{"unpinned: a single claimant needs --trust-role", []RosterEntry{holder("build-a/claude.b1", "build", time.Minute)}, nil, RoleOpts{}, "", "aivoicebus role pin build 'build-a/claude.*'", ""},
		{"unpinned: a single claimant resolves with --trust-role", []RosterEntry{holder("build-a/claude.b1", "build", time.Minute)}, nil, trust, "build-a/claude.b1", "", ""},
		{"unpinned: never for an urgent message", []RosterEntry{holder("build-a/claude.b1", "build", time.Minute)}, nil, RoleOpts{Trust: true, Urgent: true}, "", "never used for an urgent", ""},
		{"unpinned: never for a file or log link", []RosterEntry{holder("build-a/claude.b1", "build", time.Minute)}, nil, RoleOpts{Trust: true, Attach: true}, "", "never used for an urgent message or a file", ""},
		{"unpinned: a stale single claimant is how a role is taken over, even with --trust-role", []RosterEntry{holder("build-a/claude.b1", "build", 3*time.Hour)}, nil, trust, "", "not heard from", ""},
		{"unpinned: the real holder idle and an attacker live is two claimants, never the attacker", []RosterEntry{holder("build-a/claude.b1", "build", 3*time.Hour), holder("evil/claude.ffff", "build", time.Minute)}, nil, trust, "", "2 sessions", ""},
		{"unpinned: two live claimants", []RosterEntry{holder("a/claude.1111", "build", time.Minute), holder("b/claude.2222", "build", time.Hour)}, nil, trust, "", "never guessed", ""},
		{"unpinned: nobody claims it", []RosterEntry{holder("build-a/claude.b1", "review", 0)}, nil, trust, "", "no session claims @build", ""},
		{"unpinned: only this session", []RosterEntry{holder(me, "build", 0)}, nil, trust, "", "only this session", ""},
		{"unpinned: never itself, so the other one is the only claimant", []RosterEntry{holder(me, "build", 0), holder("a/claude.1111", "build", time.Minute)}, nil, trust, "a/claude.1111", "", ""},

		{"the nodes' own machine is never a holder", []RosterEntry{holder("aivoicebus/retraction", "build", 0)}, nil, trust, "", "no session claims", ""},
		{"a lookalike holder is skipped", []RosterEntry{holder("a/clаude.1111", "build", 0), holder("b/claude.2222", "build", time.Minute)}, nil, trust, "b/claude.2222", "", ""},
		{"a hyphen-lookalike holder is skipped", []RosterEntry{holder("build‑a/claude.b1", "build", 0)}, nil, trust, "", "no session claims", ""},
	}
	for _, c := range cases {
		res, err := ResolveRole("build", c.roster, c.pins, me, t0, c.o)
		switch {
		case c.errHas == "" && (err != nil || res.Target != c.target):
			t.Errorf("%s: %q %v", c.name, res.Target, err)
		case c.errHas != "" && (err == nil || !strings.Contains(err.Error(), c.errHas) || res.Target != ""):
			t.Errorf("%s: %q %v", c.name, res.Target, err)
		}
		if c.outHas != "" && (len(res.Out) == 0 || res.Out[0] != c.outHas) {
			t.Errorf("%s: ignored claims %v", c.name, res.Out)
		}
	}
}

func TestAResolvedRoleSaysWhoAndHow(t *testing.T) {
	r := []RosterEntry{holder("build-a/claude.b1", "build", time.Minute)}
	res, err := ResolveRole("build", r, nil, "me/x", t0, RoleOpts{Trust: true})
	if err != nil || res.Line() != "resolved @build -> build-a/claude.b1 (unverified)" || !strings.Contains(res.Describe(), "not verified") {
		t.Errorf("%q %q %v", res.Line(), res.Describe(), err)
	}
	res, _ = ResolveRole("build", r, map[string][]string{"build": {"build-*/*"}}, "me/x", t0, RoleOpts{})
	if res.Line() != "resolved @build -> build-a/claude.b1 (matches your pin pattern; identities are self-declared)" || !strings.Contains(res.Describe(), "pinned to build-*/*") || strings.Contains(res.Describe(), "not verified") {
		t.Error(res.Line(), res.Describe())
	}
}

// An identity with capitals is reached as the node holds it: the pin patterns are lower case, the
// target is not lowered (a lower-cased copy would be a different identity and reach nobody).
func TestARoleResolvesToTheHoldersIdentityAsItIs(t *testing.T) {
	r := []RosterEntry{holder("Box/Claude.B1", "build", time.Minute)}
	res, err := ResolveRole("build", r, map[string][]string{"build": {"box/claude.*"}}, "me/x", t0, RoleOpts{})
	if err != nil || res.Target != "Box/Claude.B1" {
		t.Errorf("%q %v", res.Target, err)
	}
}
