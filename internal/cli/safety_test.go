package cli

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/updater"
)

// typed makes the confirmation prompts answer with these lines, in order, as a person at a terminal would.
func typed(t *testing.T, lines ...string) *[]string {
	t.Helper()
	var asked []string
	old := askLine
	askLine = func(s Streams, prompt string) (string, error) {
		asked = append(asked, prompt)
		if len(lines) == 0 {
			return "", errNoTerminal
		}
		l := lines[0]
		lines = lines[1:]
		return l, nil
	}
	t.Cleanup(func() { askLine = old })
	return &asked
}

func freshHome(t *testing.T) agentcfg.Paths {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("AIVOICEBUS_URL", "")
	return agentcfg.Paths{Home: h}
}

func TestHelpListsEveryCommandAndNothingElse(t *testing.T) {
	listed := map[string]bool{}
	for _, tp := range helpTopics {
		listed[tp.name] = true
		if tp.summary == "" {
			t.Errorf("%s has no summary", tp.name)
		}
		if _, ok := commands[tp.name]; !ok {
			t.Errorf("help lists %q, which is not a command", tp.name)
		}
	}
	for name := range commands {
		if !listed[name] {
			t.Errorf("command %q is missing from the help list (internal/cli/help.go)", name)
		}
	}
}

func TestHelpFlagsAndTheHelpCommand(t *testing.T) {
	for _, args := range [][]string{{"-h"}, {"--help"}, {"-help"}, {"help"}} {
		code, out, e := run(t, "", args...)
		if code != 0 || !strings.Contains(out, "usage: aivoicebus <command>") || !strings.Contains(out, "uninstall") || e != "" {
			t.Errorf("%v: code=%d err=%q out=%q", args, code, e, out)
		}
	}
	if code, out, _ := run(t, "", "help", "send"); code != 0 || !strings.Contains(out, "send:") || !strings.Contains(out, "aivoicebus send -h") {
		t.Errorf("help send: %d %q", code, out)
	}
	if code, _, e := run(t, "", "help", "frobnicate"); code == 0 || !strings.Contains(e, "unknown command") {
		t.Errorf("help frobnicate: %d %q", code, e)
	}
}

func TestSetupNeverEditsOutsideItsDirectoryWithoutConsent(t *testing.T) {
	p := freshHome(t)
	code, _, e := run(t, "", "setup", "--url", "http://127.0.0.1:7777")
	if code == 0 || !strings.Contains(e, "no terminal to confirm on") || !strings.Contains(e, "--yes") {
		t.Fatalf("code=%d err=%q", code, e)
	}
	for _, f := range []string{p.Settings(), p.Instructions(agentcfg.Tools[0]), p.Zshrc(), p.State(), p.Env()} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written without consent", f)
		}
	}
}

func TestSetupAsksOnceAndRespectsANo(t *testing.T) {
	p := freshHome(t)
	asked := typed(t, "n")
	code, out, e := run(t, "", "setup", "--url", "http://127.0.0.1:7777")
	if code == 0 || !strings.Contains(e, "cancelled") || len(*asked) != 1 || !strings.Contains(out, "setup would change") {
		t.Fatalf("code=%d err=%q asked=%v out=%q", code, e, *asked, out)
	}
	if _, err := os.Stat(p.Settings()); err == nil {
		t.Error("a no still wrote")
	}
	asked = typed(t, "y")
	if code, _, e := run(t, "", "setup", "--url", "http://127.0.0.1:7777"); code != 0 {
		t.Fatalf("a yes: %d %q", code, e)
	}
	if _, err := os.Stat(p.State()); err != nil {
		t.Fatal("setup did not run after a yes")
	}
	// the machine has been set up with consent: refreshing it does not ask again
	asked = typed(t)
	if code, _, e := run(t, "", "setup"); code != 0 || len(*asked) != 0 {
		t.Errorf("a refresh asked: code=%d asked=%v err=%q", code, *asked, e)
	}
}

func TestSetupYesAndDryRunNeedNoAnswer(t *testing.T) {
	p := freshHome(t)
	asked := typed(t)
	if code, _, e := run(t, "", "setup", "--dry-run"); code != 0 || len(*asked) != 0 {
		t.Errorf("dry run: %d %q", code, e)
	}
	if _, err := os.Stat(p.State()); err == nil {
		t.Error("a dry run wrote")
	}
	if code, _, e := run(t, "", "setup", "--yes", "--url", "http://127.0.0.1:7777"); code != 0 || len(*asked) != 0 {
		t.Errorf("--yes: %d %q", code, e)
	}
	if _, err := os.Stat(p.State()); err != nil {
		t.Error("--yes did not run setup")
	}
}

func TestSetupRefusesAPlainHTTPNodeOnTheOpenInternet(t *testing.T) {
	p := freshHome(t)
	old := lookupIP
	lookupIP = func(host string) ([]net.IP, error) { return []net.IP{net.ParseIP("203.0.113.9")}, nil }
	t.Cleanup(func() { lookupIP = old })
	for _, u := range []string{"http://example.com:7777", "http://203.0.113.9:7777", "http://10.0.0.1:7777,http://203.0.113.9:7777"} {
		code, _, e := run(t, "", "setup", "--yes", "--url", u)
		if code == 0 || !strings.Contains(e, "plain http") {
			t.Errorf("%s: code=%d err=%q", u, code, e)
		}
	}
	if _, err := os.Stat(p.Env()); err == nil {
		t.Error("an env file was written for a refused URL")
	}
	for _, u := range []string{"https://example.com", "http://127.0.0.1:7777", "http://10.0.0.1:7777", "http://100.64.0.12:7777"} {
		if code, _, e := run(t, "", "setup", "--yes", "--url", u); code != 0 {
			t.Errorf("%s: code=%d err=%q", u, code, e)
		}
	}
}

func TestUninstallAsksThenRemovesAndDryRunOnlyShows(t *testing.T) {
	p := freshHome(t)
	if code, _, e := run(t, "", "setup", "--yes", "--url", "http://127.0.0.1:7777"); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	code, out, _ := run(t, "", "uninstall", "--dry-run")
	if code != 0 || !strings.Contains(out, "would change") {
		t.Fatalf("dry run: %d %q", code, out)
	}
	if _, err := os.Stat(p.State()); err != nil {
		t.Fatal("a dry run removed")
	}
	if code, _, e := run(t, "", "uninstall"); code == 0 || !strings.Contains(e, "no terminal to confirm on") {
		t.Errorf("without a terminal or --yes: %d %q", code, e)
	}
	typed(t, "no")
	if code, _, e := run(t, "", "uninstall"); code == 0 || !strings.Contains(e, "cancelled") {
		t.Errorf("a no: %d %q", code, e)
	}
	if _, err := os.Stat(p.State()); err != nil {
		t.Fatal("a no removed")
	}
	typed(t, "yes")
	code, out, e := run(t, "", "uninstall")
	if code != 0 || !strings.Contains(out, "claude hook") {
		t.Fatalf("a yes: %d %q %q", code, out, e)
	}
	if _, err := os.Stat(p.State()); err == nil {
		t.Error("state.json stayed")
	}
	if code, out, _ := run(t, "", "uninstall", "--yes"); code != 0 || !strings.Contains(out, "nothing to remove") {
		t.Errorf("a second run: %d %q", code, out)
	}
}

func TestUninstallYesPurgeRemovesTheConfigDirectory(t *testing.T) {
	p := freshHome(t)
	run(t, "", "setup", "--yes", "--url", "http://127.0.0.1:7777")
	os.WriteFile(filepath.Join(p.ConfigDir(), "token"), []byte("x"), 0o600)
	if code, _, e := run(t, "", "uninstall", "--yes", "--purge"); code != 0 {
		t.Fatalf("%d %q", code, e)
	}
	if _, err := os.Stat(p.ConfigDir()); err == nil {
		t.Error("--purge left the config directory")
	}
}

func TestTrustKeyAddNeedsTheFingerprintTypedOrGiven(t *testing.T) {
	freshHome(t)
	_, pub, _ := release.GenerateKey()
	key, fp := release.EncodeKey(pub), release.Fingerprint(pub)

	if code, _, e := run(t, "", "trust-key", "add", key); code == 0 || !strings.Contains(e, "needs a terminal") {
		t.Errorf("no terminal, no fingerprint: %d %q", code, e)
	}
	if code, _, e := run(t, "", "trust-key", "add", key, "--fingerprint", "SHA256:wrong"); code == 0 || !strings.Contains(e, "does not match") {
		t.Errorf("wrong fingerprint: %d %q", code, e)
	}
	typed(t, "SHA256:typed-wrong")
	if code, _, e := run(t, "", "trust-key", "add", key); code == 0 || !strings.Contains(e, "not the fingerprint") {
		t.Errorf("a wrong typed fingerprint: %d %q", code, e)
	}
	if _, err := os.Stat(release.UserKeyFile()); err == nil {
		t.Fatal("a key was written without a confirmed fingerprint")
	}

	if code, out, e := run(t, "", "trust-key", "add", key, "--fingerprint", fp, "--label", "my mesh"); code != 0 || !strings.Contains(out, "added") {
		t.Fatalf("add: %d %q %q", code, out, e)
	}
	_, out, _ := run(t, "", "trust-key", "list")
	if !strings.Contains(out, fp) || !strings.Contains(out, "my mesh") || !strings.Contains(out, "built in") {
		t.Errorf("list: %q", out)
	}
	if code, out, _ := run(t, "", "trust-key", "add", key, "--fingerprint", fp); code != 0 || !strings.Contains(out, "already trusted") {
		t.Errorf("a second add: %d %q", code, out)
	}
	if code, _, e := run(t, "", "trust-key", "rm", fp); code != 0 {
		t.Errorf("rm: %d %q", code, e)
	}
	if _, out, _ := run(t, "", "trust-key", "list"); strings.Contains(out, fp) {
		t.Errorf("the key stayed: %q", out)
	}
}

func TestTrustKeyAddByTypingTheFingerprint(t *testing.T) {
	freshHome(t)
	_, pub, _ := release.GenerateKey()
	typed(t, release.Fingerprint(pub))
	if code, out, e := run(t, "", "trust-key", "add", release.EncodeKey(pub)); code != 0 || !strings.Contains(out, "added") {
		t.Fatalf("%d %q %q", code, out, e)
	}
}

func TestTrustKeyCannotRemoveABuiltInKey(t *testing.T) {
	freshHome(t)
	ks, _ := release.Keys()
	if code, _, e := run(t, "", "trust-key", "rm", release.Fingerprint(ks[0].Public)); code == 0 || !strings.Contains(e, "built-in keys cannot be removed") {
		t.Errorf("%d %q", code, e)
	}
}

func TestCheckListenAddr(t *testing.T) {
	for addr, want := range map[string]string{ // "" ok, "warn" allowed with a warning, "refuse" needs --allow-public
		"127.0.0.1:7777":    "",
		"[::1]:7777":        "",
		"localhost:7777":    "",
		"10.1.2.3:7777":     "warn",
		"192.168.0.5:7777":  "warn",
		"100.64.0.12:7777":  "warn",
		"169.254.1.1:7777":  "warn",
		"0.0.0.0:7777":      "refuse",
		":7777":             "refuse",
		"[::]:7777":         "refuse",
		"203.0.113.9:7777":  "refuse",
		"node.example:7777": "refuse",
	} {
		w, err := checkListenAddr(addr, false)
		switch want {
		case "":
			if w != "" || err != nil {
				t.Errorf("%s: %q %v", addr, w, err)
			}
		case "warn":
			if w == "" || err != nil || !strings.Contains(w, "plain HTTP") {
				t.Errorf("%s: %q %v", addr, w, err)
			}
		case "refuse":
			if err == nil || !strings.Contains(err.Error(), "--allow-public") {
				t.Errorf("%s: %q %v", addr, w, err)
			}
			if w, err := checkListenAddr(addr, true); err != nil || !strings.Contains(w, "plain HTTP") {
				t.Errorf("%s with --allow-public: %q %v", addr, w, err)
			}
		}
	}
	if _, err := checkListenAddr("7777", false); err == nil {
		t.Error("an address without a host:port accepted")
	}
}

func TestServeRefusesToListenOnTheOpenInternetWithoutAllowPublic(t *testing.T) {
	freshHome(t)
	code, _, e := run(t, "", "serve", "--addr", "0.0.0.0:0", "--data", t.TempDir())
	if code == 0 || !strings.Contains(e, "--allow-public") {
		t.Errorf("%d %q", code, e)
	}
}

func TestUpdateReadsFromGitHubWhenNoMeshIsConfigured(t *testing.T) {
	freshHome(t)
	src, err := releaseSource("auto")
	if err != nil {
		t.Fatal(err)
	}
	if g, ok := src.(*updater.GitHubSource); !ok || g.Repo != githubRepo {
		t.Errorf("%T", src)
	}
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:9")
	t.Setenv("AIVOICEBUS_TOKEN", "tok")
	src, err = releaseSource("auto")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := src.(updater.MeshSource); !ok {
		t.Errorf("a configured mesh should be preferred: %T", src)
	}
}

func TestNodeListsAndReleasesSignedByAUserTrustedKeyAreAccepted(t *testing.T) {
	freshHome(t)
	dir, pub := signedRelease(t, "9.9.9")
	if _, err := release.VerifyDir(dir, release.TrustedKeys()); err == nil {
		t.Fatal("a stranger's key is trusted by default")
	}
	if _, err := release.AddUserKey(pub, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := release.VerifyDir(dir, release.TrustedKeys()); err != nil {
		t.Errorf("a release signed by a key the user added: %v", err)
	}
}
