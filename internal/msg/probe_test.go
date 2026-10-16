package msg

import "testing"

func TestIsProbe(t *testing.T) {
	for id, want := range map[string]bool{
		"box/join-probe": true, "box/join-reader": true, "mac-m1-mbp/join-reader": true,
		"box/claude.1": false, "join-probe": false, "box/join-probe.2": false, "": false,
	} {
		if got := IsProbe(id); got != want {
			t.Errorf("IsProbe(%q) = %v, want %v", id, got, want)
		}
	}
}

// The names rules (lookalike lint, reserved words) must never refuse the identities join probes
// with, and a lookalike of one is no probe: it stays visible and is refused as a sender.
func TestProbeIdentitiesPassTheNamesRulesAndLookalikesAreNotProbes(t *testing.T) {
	for _, host := range []string{"testbox", "build-a", "laptop-a", "imac.local"} {
		for _, agent := range []string{ProbeSenderAgent, ProbeReaderAgent} {
			id := host + "/" + agent
			if !IsProbe(id) || SenderProblem(id) != "" || TargetProblem(id) != "" || NameProblem(id) != "" || !ValidIdentity(id) {
				t.Errorf("%s: probe %v, sender %q, target %q, name %q, valid %v", id, IsProbe(id), SenderProblem(id), TargetProblem(id), NameProblem(id), ValidIdentity(id))
			}
		}
	}
	for _, fake := range []string{"box/join-prоbe", "box/join‑probe", "box/join-probe​", "box/join-readeŕ"} {
		if IsProbe(fake) {
			t.Errorf("%q is not a probe", fake)
		}
		if SenderProblem(fake) == "" && NameProblem(fake) == "" {
			t.Errorf("%q passes for a probe and is not refused by the names rules", fake)
		}
	}
}

// The probe agent names cannot be registered as a long-lived name or claimed as a role.
func TestProbeAgentNamesAreReservedForNamesAndRoles(t *testing.T) {
	for _, a := range []string{ProbeSenderAgent, ProbeReaderAgent} {
		if ValidRoleName(a) {
			t.Errorf("%s is a valid role", a)
		}
		if err := ValidPersistentName(a + ".build1"); err == nil {
			t.Errorf("%s.build1 is a valid persistent name", a)
		}
		if err := ValidPersistentName("codex." + a); err == nil {
			t.Errorf("codex.%s is a valid persistent name", a)
		}
	}
}
