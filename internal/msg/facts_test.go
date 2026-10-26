package msg

import (
	"slices"
	"strings"
	"testing"
)

// A fact's version is advertised only when it can be nothing but a version: whatever a version
// file or a --version printed instead (a key, a path, a sentence) is dropped.
func TestCleanVersionKeepsOnlyVersions(t *testing.T) {
	for in, want := range map[string]string{
		"0.3.13":                     "0.3.13",
		"v0.3.13\n":                  "0.3.13",
		"fleet-ctx 0.2.0":            "0.2.0",
		"0.13.1-rc1":                 "0.13.1-rc1",
		"1":                          "1",
		"":                           "",
		"/Users/live/x":              "",
		"0.3.13+build7":              "",
		"-----BEGIN KEY---":          "",
		"c2lnbmluZy1zZWVk":           "",
		"0.3.13 trailing":            "",
		"12345678901234567890.1.2.3": "",
	} {
		if got := CleanVersion(in); got != want {
			t.Errorf("CleanVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHWordLeavesTheBastionSuffixOffAndRefusesWhatIsNoAlias(t *testing.T) {
	for in, want := range map[string]string{
		"0own_bastion":   "0own",
		"0MCP_Bastion":   "0mcp",
		"node-c":         "node-c",
		"_bastion":       "",
		"-oProxyCommand": "",
		"a b":            "",
		"~/.ssh/config":  "",
		"host/x":         "",
	} {
		if got := SSHWord(in); got != want {
			t.Errorf("SSHWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// The first words survive a cut, whole; nothing at all is "none", which still says "I looked".
func TestFactValueKeepsTheOrderAndCutsAtAWholeWord(t *testing.T) {
	if got := FactValue([]string{"signing-key", "deploy-token", "fleet-guard-0.3.13"}); got != "signing-key+deploy-token+fleet-guard-0.3.13" {
		t.Errorf("order: %q", got)
	}
	if got := FactValue(nil); got != FactNone {
		t.Errorf("no words: %q", got)
	}
	if got := FactValue([]string{"a b", "", "ok"}); got != "ok" {
		t.Errorf("words that are no words are dropped: %q", got)
	}
	long := []string{"signing-key", strings.Repeat("x", 40), strings.Repeat("y", 20)}
	if got := FactValue(long); got != "signing-key+"+strings.Repeat("x", 40) {
		t.Errorf("cut at a whole word within %d: %q", MaxLabelValue, got)
	}
}

func TestFactsOfReadsTheLabelsAndSaysWhenNothingWasSaid(t *testing.T) {
	if f := FactsOf(Labels{"repo": "x", SSHLabel: "0own"}); f.Known {
		t.Errorf("no has label: nothing was said, an ssh label alone does not count: %+v", f)
	}
	f := FactsOf(Labels{HasLabel: "signing-key+fleet-guard-0.3.13+fleet-ctx-bad+junk", SSHLabel: "0own+0mcp+0own"})
	if !f.Known || !f.SigningKey || f.DeployToken || f.Tools[FactFleetGuard] != "0.3.13" || f.Tools[FactFleetCtx] != "" || !slices.Equal(f.SSH, []string{"0mcp", "0own"}) {
		t.Fatalf("%+v", f)
	}
	if got := strings.Join(f.Words(), " "); got != "signing-key fleet-guard:0.3.13 ssh:0mcp,0own" {
		t.Errorf("words: %q", got)
	}
	if f := FactsOf(Labels{HasLabel: FactNone}); !f.Known || len(f.Words()) != 0 {
		t.Errorf("none: said, and has nothing: %+v", f)
	}
}

func TestParseFactQueryAcceptsTheFactsAndExplainsTheRest(t *testing.T) {
	for in, want := range map[string]FactQuery{
		"signing-key":          {Name: FactSigningKey},
		"Release-Signing.Key":  {Name: FactSigningKey},
		"deploy-token":         {Name: FactDeployToken},
		"fleet-guard":          {Name: FactFleetGuard},
		"fleet-guard:0.3.*":    {Name: FactFleetGuard, Pattern: "0.3.*"},
		"fleet-ctx:0.2.0":      {Name: FactFleetCtx, Pattern: "0.2.0"},
		"ssh:0own_bastion":     {Name: FactSSH, Pattern: "0own"},
		"ssh:0own":             {Name: FactSSH, Pattern: "0own"},
		"ssh:*":                {Name: FactSSH, Pattern: "*"},
		" ssh:node-c_bastion ": {Name: FactSSH, Pattern: "node-c"},
	} {
		got, err := ParseFactQuery(in)
		if err != nil || got.Name != want.Name || got.Pattern != want.Pattern {
			t.Errorf("ParseFactQuery(%q) = %+v, %v; want %+v", in, got, err, want)
		}
		if got.String() != strings.ToLower(strings.TrimSpace(in)) {
			t.Errorf("%q is shown as %q: as typed", in, got.String())
		}
	}
	for _, in := range []string{"", "token", "signing-key:yes", "ssh", "ssh:", "ssh:a b", "fleet-guard:0.3;rm", "fleet-ctx:"} {
		_, err := ParseFactQuery(in)
		if err == nil || !strings.Contains(err.Error(), "the facts are signing-key, deploy-token") {
			t.Errorf("ParseFactQuery(%q): %v", in, err)
		}
	}
}

func TestFactQueryMatch(t *testing.T) {
	f := FactsOf(Labels{HasLabel: "deploy-token+fleet-guard-0.3.13", SSHLabel: "0docker+0own"})
	for q, want := range map[string]struct {
		yes bool
		v   string
	}{
		"signing-key":       {false, ""},
		"deploy-token":      {true, ""},
		"fleet-guard":       {true, "0.3.13"},
		"fleet-guard:0.3.*": {true, "0.3.13"},
		"fleet-guard:0.4.*": {false, "0.3.13"},
		"fleet-ctx":         {false, ""},
		"ssh:0own_bastion":  {true, "0own"},
		"ssh:0mcp_bastion":  {false, ""},
		"ssh:0d*":           {true, "0docker"},
	} {
		fq, err := ParseFactQuery(q)
		if err != nil {
			t.Fatal(err)
		}
		if yes, v := fq.Match(f); yes != want.yes || v != want.v {
			t.Errorf("%s: %v %q, want %v %q", q, yes, v, want.yes, want.v)
		}
	}
}

func TestValidFactsAcceptsOnlyWhatCouldHaveBeenWorkedOut(t *testing.T) {
	for _, ok := range [][2]string{{"none", ""}, {"signing-key+fleet-ctx-0.2.0", "0mcp+0own"}, {"deploy-token", "builder"}} {
		if !ValidFacts(ok[0], ok[1]) {
			t.Errorf("%q %q is valid", ok[0], ok[1])
		}
	}
	for _, bad := range [][2]string{{"", ""}, {"/home/me/.config/aivoicebus/token", ""}, {"signing-key+secret", ""}, {"none", "0own_bastion"}, {"none", "a b"}, {"none", "0own++0mcp"}, {"fleet-guard-x", ""}} {
		if ValidFacts(bad[0], bad[1]) {
			t.Errorf("%q %q is not", bad[0], bad[1])
		}
	}
}
