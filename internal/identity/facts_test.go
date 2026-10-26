package identity

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// fakeMachine is a prober over a made-up home: files (path -> contents, relative to home), the
// commands it may run (fleet-ctx --version, ssh -G), and a record of every path stat and readHead
// were given.
type fakeMachine struct {
	home   string
	files  map[string]string
	ssh    map[string]string // alias -> the hostname ssh -G prints
	ctx    string            // what fleet-ctx --version prints; "" when it is not installed
	noPATH bool              // fleet-ctx is not on PATH (a hook's PATH may lack ~/.local/bin)
	stats  []string
	reads  []string
	runs   []string
	probes int
	mu     sync.Mutex // the ssh -G checks run side by side
}

type fakeInfo struct{ mode fs.FileMode }

func (fakeInfo) Name() string        { return "" }
func (fakeInfo) Size() int64         { return 0 }
func (i fakeInfo) Mode() fs.FileMode { return i.mode }
func (fakeInfo) ModTime() time.Time  { return time.Time{} }
func (i fakeInfo) IsDir() bool       { return i.mode.IsDir() }
func (fakeInfo) Sys() any            { return nil }
func (m *fakeMachine) rel(p string) string {
	r, _ := filepath.Rel(m.home, p)
	return r
}

func (m *fakeMachine) prober() prober {
	m.probes++
	return prober{
		home: m.home,
		stat: func(p string) (os.FileInfo, error) {
			m.stats = append(m.stats, m.rel(p))
			if _, ok := m.files[m.rel(p)]; ok {
				return fakeInfo{0o755}, nil
			}
			return nil, fs.ErrNotExist
		},
		readHead: func(p string, n int64) ([]byte, error) {
			m.reads = append(m.reads, m.rel(p))
			c, ok := m.files[m.rel(p)]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return []byte(c[:min(int64(len(c)), n)]), nil
		},
		run: func(name string, args ...string) ([]byte, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			m.runs = append(m.runs, name+" "+strings.Join(args, " "))
			switch {
			case strings.HasSuffix(name, "fleet-ctx") && m.ctx != "":
				return []byte(m.ctx), nil
			case name == "ssh" && len(args) == 2 && args[0] == "-G":
				h := m.ssh[args[1]]
				if h == "" {
					h = strings.ToLower(args[1]) // ssh's answer for an alias it does not know
				}
				return []byte("user me\nhostname " + h + "\nport 22\n"), nil
			}
			return nil, errors.New("not found")
		},
		lookPath: func(f string) (string, error) {
			if f == "fleet-ctx" && m.ctx != "" && !m.noPATH {
				return "/usr/local/bin/fleet-ctx", nil
			}
			return "", errors.New("not found")
		},
		glob: func(pat string) ([]string, error) {
			var out []string
			for p := range m.files {
				if ok, _ := filepath.Match(pat, filepath.Join(m.home, p)); ok {
					out = append(out, filepath.Join(m.home, p))
				}
			}
			return out, nil
		},
	}
}

// useMachine makes Labels and MachineFacts see m, with the cache in a fresh directory.
func useMachine(t *testing.T, m *fakeMachine) {
	t.Helper()
	t.Setenv("HOME", m.home)
	t.Setenv("AIVOICEBUS_FACT_SSH", "")
	t.Setenv("AIVOICEBUS_FACTS", "")
	old := newProber
	t.Cleanup(func() { newProber = old })
	newProber = m.prober
}

const secret = "c2VjcmV0LXNlZWQtZG8tbm90LXNlbmQ" // what a key file holds: it must never be read or sent

func keyMachine(t *testing.T) *fakeMachine {
	return &fakeMachine{
		home: t.TempDir(),
		files: map[string]string{
			".config/aivoicebus/release-signing.key": secret,
			".config/aivoicebus/deploy-token":        secret,
			".fleet-guard/VERSION":                   "0.3.13\n",
			".ssh/config": "Include config.d/*.conf\nHost 0own_bastion 0own_bastion_tailscale *_bastion !bad_bastion\n" +
				"  HostName 192.0.2.1\nHost=builder\n",
			".ssh/config.d/a.conf":    "host \"0mcp_bastion\"\nHost 0gone_bastion\n# Host 0commented_bastion\n",
			".ssh/config.d/b.notconf": "Host 0skipped_bastion\n",
		},
		ssh: map[string]string{"0own_bastion": "192.0.2.1", "0mcp_bastion": "198.51.100.7", "builder": "192.0.2.9"},
		ctx: "fleet-ctx 0.2.0\n",
	}
}

// The facts come from the allowlist and nothing else: the two secret files are only ever looked
// at (stat), never opened; the ssh aliases are the Host names ending in _bastion of the ssh config
// and the files it includes, checked with ssh -G (one ssh does not know is not counted).
func TestFactsComeFromTheAllowlistAndASecretIsNeverOpened(t *testing.T) {
	m := keyMachine(t)
	has, ssh := m.prober().facts("")
	if has != "signing-key+deploy-token+fleet-guard-0.3.13+fleet-ctx-0.2.0" {
		t.Errorf("has: %q", has)
	}
	if ssh != "0mcp+0own" {
		t.Errorf("ssh: %q (0gone_bastion is in the config but resolves to nothing; builder does not end in _bastion; b.notconf is not included)", ssh)
	}
	for _, r := range m.reads {
		if strings.HasSuffix(r, "release-signing.key") || strings.HasSuffix(r, "deploy-token") {
			t.Errorf("a secret file was opened: %s (reads: %v)", r, m.reads)
		}
	}
	for _, want := range []string{".config/aivoicebus/release-signing.key", ".config/aivoicebus/deploy-token"} {
		if !strings.Contains(strings.Join(m.stats, " "), want) {
			t.Errorf("%s was not looked at: %v", want, m.stats)
		}
	}
	for _, r := range m.runs {
		if r != "/usr/local/bin/fleet-ctx --version" && !strings.HasPrefix(r, "ssh -G 0") {
			t.Errorf("ran something outside the allowlist: %q", r)
		}
	}
	if strings.Contains(has+ssh, secret) {
		t.Fatal("a secret was advertised")
	}
}

// A version file or command that prints something other than a version advertises no version.
func TestAToolThatPrintsNoVersionAdvertisesNone(t *testing.T) {
	m := keyMachine(t)
	m.files[".fleet-guard/VERSION"] = "/home/me/.ssh/id_ed25519\n0.3.13\n"
	m.ctx = "/home/me/.local/bin/fleet-ctx: permission denied"
	has, _ := m.prober().facts("")
	if has != "signing-key+deploy-token" {
		t.Errorf("has: %q", has)
	}
	m.files[".fleet-guard/VERSION"] = secret
	if has, _ := m.prober().facts(""); has != "signing-key+deploy-token" {
		t.Errorf("has: %q", has)
	}
}

func TestAMachineWithNothingSaysNone(t *testing.T) {
	m := &fakeMachine{home: t.TempDir(), files: map[string]string{".config/aivoicebus/token": secret}}
	has, ssh := m.prober().facts("")
	if has != msg.FactNone || ssh != "" {
		t.Errorf("has %q ssh %q", has, ssh)
	}
	if len(m.reads) != 2 { // fleet-guard's VERSION and the ssh config, both missing
		t.Errorf("reads: %v", m.reads)
	}
}

// A hook's PATH may lack ~/.local/bin, where fleet-ctx is installed: it is looked for there
// too.
func TestFleetCtxIsFoundInLocalBinWhenPATHLacksIt(t *testing.T) {
	m := keyMachine(t)
	m.noPATH = true
	if has, _ := m.prober().facts(""); strings.Contains(has, "fleet-ctx") {
		t.Fatalf("not installed anywhere: %q", has)
	}
	m.files[".local/bin/fleet-ctx"] = "#!/bin/sh"
	if has, _ := m.prober().facts(""); !strings.HasSuffix(has, "+fleet-ctx-0.2.0") {
		t.Errorf("in ~/.local/bin: %q (ran %v)", has, m.runs)
	}
}

// AIVOICEBUS_FACT_SSH names the aliases to check instead of the scan; off checks none.
func TestFactSSHNamesTheAliasesInsteadOfTheConfig(t *testing.T) {
	m := keyMachine(t)
	if _, ssh := m.prober().facts("builder, 0mcp_bastion,-oProxyCommand=x"); ssh != "0mcp+builder" {
		t.Errorf("named: %q", ssh)
	}
	for _, r := range m.runs {
		if strings.Contains(r, "-oProxyCommand") {
			t.Errorf("a name that is no alias went to ssh: %q", r)
		}
	}
	m.runs = nil
	if _, ssh := m.prober().facts("off"); ssh != "" || strings.Contains(strings.Join(m.runs, ","), "ssh") {
		t.Errorf("off: %q, ran %v", ssh, m.runs)
	}
}

func TestFactsAreWorkedOutAtMostEveryFewMinutes(t *testing.T) {
	m := keyMachine(t)
	useMachine(t, m)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := factsNow
	t.Cleanup(func() { factsNow = old })
	factsNow = func() time.Time { return now }

	if has, ssh := MachineFacts(); !strings.HasPrefix(has, "signing-key") || ssh != "0mcp+0own" || m.probes != 1 {
		t.Fatalf("first: %q %q after %d probes", has, ssh, m.probes)
	}
	now = now.Add(FactsTTL - time.Second)
	delete(m.files, ".config/aivoicebus/release-signing.key")
	if has, _ := MachineFacts(); !strings.HasPrefix(has, "signing-key") || m.probes != 1 {
		t.Fatalf("within %s the kept answer is used: %q after %d probes", FactsTTL, has, m.probes)
	}
	now = now.Add(2 * time.Second)
	if has, _ := MachineFacts(); has != "deploy-token+fleet-guard-0.3.13+fleet-ctx-0.2.0" || m.probes != 2 {
		t.Fatalf("after %s they are worked out again: %q after %d probes", FactsTTL, has, m.probes)
	}
	t.Setenv("AIVOICEBUS_FACT_SSH", "builder")
	if _, ssh := MachineFacts(); ssh != "builder" || m.probes != 3 {
		t.Fatalf("a changed AIVOICEBUS_FACT_SSH is a reason to look again: %q after %d probes", ssh, m.probes)
	}
	now = now.Add(-time.Hour) // the clock went back: an answer from the future is not trusted
	MachineFacts()
	if m.probes != 4 {
		t.Fatalf("probes: %d", m.probes)
	}
	b, err := os.ReadFile(FactsCache())
	if err != nil || strings.Contains(string(b), secret) {
		t.Fatalf("the cache: %s %v", b, err)
	}
}

// What the cache file says goes out as labels, so one that does not read as facts is worked out again.
func TestACacheThatIsNotFactsIsNotUsed(t *testing.T) {
	m := keyMachine(t)
	useMachine(t, m)
	for _, bad := range []string{
		`{"at":"%s","has":"/home/me/.config/aivoicebus/token"}`,
		`{"at":"%s","has":"signing-key","ssh":"a b"}`,
		`{"at":"%s","has":""}`,
		`not json`,
	} {
		os.MkdirAll(filepath.Dir(FactsCache()), 0o700)
		if strings.Contains(bad, "%s") {
			bad = strings.Replace(bad, "%s", time.Now().UTC().Format(time.RFC3339Nano), 1)
		}
		os.WriteFile(FactsCache(), []byte(bad), 0o600)
		before := m.probes
		if has, _ := MachineFacts(); !strings.HasPrefix(has, "signing-key+deploy-token") || m.probes != before+1 {
			t.Errorf("%s: %q after %d probes", bad, has, m.probes-before)
		}
	}
}

func labelsOn(t *testing.T, m *fakeMachine, pinned string) msg.Labels {
	t.Helper()
	useMachine(t, m)
	fakeGit(t, "", "")
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "claude")
	t.Setenv("AIVOICEBUS_HOST", "keymac")
	t.Setenv("AIVOICEBUS_LABELS", pinned)
	t.Setenv("AIVOICEBUS_CAPS", "")
	t.Setenv("AIVOICEBUS_ROLE", "")
	return Labels("/anywhere")
}

// The labels carry the facts this machine worked out, replacing any pinned ones (which are stale).
func TestLabelsCarryTheMachinesFactsAndReplacePinnedOnes(t *testing.T) {
	m := keyMachine(t)
	l := labelsOn(t, m, "has=none,ssh=0zzz")
	if l[msg.HasLabel] != "signing-key+deploy-token+fleet-guard-0.3.13+fleet-ctx-0.2.0" || l[msg.SSHLabel] != "0mcp+0own" {
		t.Fatalf("%v", l)
	}
	if strings.Contains(l.String(), secret) {
		t.Fatal("a secret is in the labels")
	}
	m.ssh = nil // no alias resolves any more: the pinned ssh= must not stand in for the answer
	os.Remove(FactsCache())
	if l := labelsOn(t, m, "has=none,ssh=0zzz"); l[msg.SSHLabel] != "" || l[msg.HasLabel] == msg.FactNone {
		t.Fatalf("%v", l)
	}
}

// Facts never push out another label, and come both or neither: a session that says has= has
// said everything, so a missing ssh label means no alias, never "did not fit".
func TestFactsGoOnlyIntoRoomThatIsLeftAndBothOrNeither(t *testing.T) {
	m := keyMachine(t)
	base := len(labelsOn(t, m, "")) - 2 // machine, tool, host: what this test machine says besides its facts
	var pins []string
	for i := 0; len(pins) < msg.MaxLabels-base-2; i++ {
		pins = append(pins, "p"+string(rune('a'+i))+"=x")
	}
	// the pins leave two places: has and ssh both fit
	if l := labelsOn(t, m, strings.Join(pins, ",")); l[msg.HasLabel] == "" || l[msg.SSHLabel] == "" || len(l) > msg.MaxLabels {
		t.Fatalf("two places: %v", l)
	}
	pins = append(pins, "zz=keep")
	l := labelsOn(t, m, strings.Join(pins, ","))
	if l[msg.HasLabel] != "" || l[msg.SSHLabel] != "" || l["zz"] != "keep" {
		t.Fatalf("one place and two facts labels: neither, and nothing pushed out: %v", l)
	}
	delete(m.ssh, "0own_bastion")
	delete(m.ssh, "0mcp_bastion")
	os.Remove(FactsCache())
	if l := labelsOn(t, m, strings.Join(pins, ",")); l[msg.HasLabel] == "" || l[msg.SSHLabel] != "" || len(l) != msg.MaxLabels {
		t.Fatalf("no alias resolves: has alone fits the one place: %v", l)
	}
}

// A stale pinned has= or ssh= (an earlier shell's) is dropped before anything else is laid on, so it
// never takes the place of a label routing needs.
func TestAStalePinnedFactNeverTakesAnotherLabelsPlace(t *testing.T) {
	m := keyMachine(t)
	base := len(labelsOn(t, m, "")) - 2
	pins := []string{"has=none", "ssh=0zzz"}
	for i := 0; base+len(pins) < msg.MaxLabels; i++ {
		pins = append(pins, "p"+string(rune('a'+i))+"=x")
	}
	labelsOn(t, m, strings.Join(pins, ","))
	t.Setenv("AIVOICEBUS_CAPS", "ask")
	if l := Labels("/anywhere"); l["tool"] != "claude" || l[msg.CapsLabel] != "ask" {
		t.Fatalf("the tool and the caps must survive a stale pinned fact: %v", l)
	}
}

// Under an AIVOICEBUS_ME identity nothing is said about this machine (a pinned value stands, as
// any pinned label does); AIVOICEBUS_FACTS=off says nothing at all.
func TestAnOverriddenIdentityOrFactsOffSaysNothing(t *testing.T) {
	m := keyMachine(t)
	labelsOn(t, m, "")
	before := m.probes
	t.Setenv("AIVOICEBUS_ME", "other/claude.a1")
	if l := Labels("/anywhere"); l[msg.HasLabel] != "" || m.probes != before {
		t.Errorf("AIVOICEBUS_ME: %v after %d probes", l, m.probes-before)
	}
	t.Setenv("AIVOICEBUS_LABELS", "has=signing-key")
	if l := Labels("/anywhere"); l[msg.HasLabel] != "signing-key" {
		t.Errorf("a pinned value under AIVOICEBUS_ME stands: %v", l)
	}
	t.Setenv("AIVOICEBUS_FACTS", "off")
	if l := Labels("/anywhere"); l[msg.HasLabel] != "" {
		t.Errorf("off: %v", l)
	}
	t.Setenv("AIVOICEBUS_ME", "")
	if l := Labels("/anywhere"); l[msg.HasLabel] != "" || l[msg.SSHLabel] != "" {
		t.Errorf("off: %v", l)
	}
}

// End to end on real files: with the key and the token unreadable, they still count (they are
// looked at, not opened), and their contents appear in no label and not in the cache.
func TestRealFilesAreLookedAtNeverReadOrSent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", t.TempDir()) // no fleet-ctx, no ssh: what an empty machine has
	t.Setenv("AIVOICEBUS_FACT_SSH", "")
	t.Setenv("AIVOICEBUS_FACTS", "")
	cfg := filepath.Join(home, ".config", "aivoicebus")
	os.MkdirAll(cfg, 0o700)
	os.MkdirAll(filepath.Join(home, ".fleet-guard"), 0o700)
	os.WriteFile(filepath.Join(home, ".fleet-guard", "VERSION"), []byte(home+"/.ssh/id_ed25519\n"), 0o644)
	for _, f := range []string{"release-signing.key", "deploy-token"} {
		p := filepath.Join(cfg, f)
		if err := os.WriteFile(p, []byte(secret+"\n"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	if os.Geteuid() != 0 {
		if _, err := os.ReadFile(filepath.Join(cfg, "deploy-token")); err == nil {
			t.Fatal("mode 000 did not stop a read")
		}
	}
	fakeGit(t, "", "")
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "claude")
	t.Setenv("AIVOICEBUS_HOST", "keymac")
	t.Setenv("AIVOICEBUS_LABELS", "")
	l := Labels("/anywhere")
	if l[msg.HasLabel] != "signing-key+deploy-token" {
		t.Fatalf("labels: %v", l)
	}
	cache, _ := os.ReadFile(FactsCache())
	var c machineFacts
	if err := json.Unmarshal(cache, &c); err != nil || c.Has != l[msg.HasLabel] {
		t.Fatalf("cache: %s %v", cache, err)
	}
	if fi, err := os.Stat(FactsCache()); err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("the cache is this user's alone: %v %v", fi.Mode(), err)
	}
	for what, b := range map[string]string{"labels": l.String(), "cache": string(cache)} {
		if strings.Contains(b, secret) || strings.Contains(b, home) {
			t.Errorf("%s holds a file's contents or a path: %s", what, b)
		}
	}
}
