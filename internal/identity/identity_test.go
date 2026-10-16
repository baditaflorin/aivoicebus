package identity

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestMePrecedence(t *testing.T) {
	resetDetection(t, nil) // no tool above this process, whatever runs the tests
	if got := Me(); got != Label()+"/agent" {
		t.Errorf("default: %q", got)
	}
	t.Setenv("AIVOICEBUS_AGENT", "Claude.A1B2")
	if got := Me(); got != Label()+"/claude.a1b2" {
		t.Errorf("agent is lowercased and appended to the host label: %q", got)
	}
	t.Setenv("AIVOICEBUS_ME", "IMac/Codex.Q1W2")
	if got := Me(); got != "imac/codex.q1w2" {
		t.Errorf("AIVOICEBUS_ME overrides everything: %q", got)
	}
}

func TestHostIsSafeInsideAnIdentity(t *testing.T) {
	h := Host()
	if h == "" || h != strings.ToLower(h) || strings.ContainsAny(h, " /") || strings.HasSuffix(h, ".local") {
		t.Errorf("unsafe host %q", h)
	}
}

func TestSuffixFrom(t *testing.T) {
	cases := []struct{ name, chip, model, want string }{
		{"MacBook Pro", "Apple M1 Pro", "MacBookPro18,3", "m1pro-mbp"},
		{"MacBook Pro", "Apple M4 Max", "Mac16,5", "m4max-mbp"},
		{"MacBook Air", "Apple M1", "MacBookAir10,1", "m1-mba"},
		{"MacBook Air", "Apple M4", "Mac16,12", "m4-mba"},
		{"Mac mini", "Apple M4", "Mac16,10", "m4-mini"},
		{"Mac Studio", "Apple M2 Ultra", "Laptop,14", "m2ultra-studio"},
		{"iMac", "Intel Core i7", "iMac19,1", "intel-imac"},
		{"iMac Pro", "Intel Xeon W", "iMacPro1,1", "intel-imacpro"},
		// marketing name unavailable: legacy identifiers still say what they are...
		{"", "Apple M1 Pro", "MacBookPro18,3", "m1pro-mbp"},
		{"", "Intel(R) Core(TM) i7-4770HQ", "MacBookPro11,1", "intel-mbp"},
		// ...new-style ones do not, so the product falls back to "mac"
		{"", "Apple M3", "Mac15,12", "m3-mac"},
		{"", "", "Mac15,12", "mac"},
	}
	for _, c := range cases {
		if got := suffixFrom(c.name, c.chip, c.model); got != c.want {
			t.Errorf("suffixFrom(%q,%q,%q) = %q, want %q", c.name, c.chip, c.model, got, c.want)
		}
	}
}

func TestCleanLabel(t *testing.T) {
	for in, want := range map[string]string{
		"Laptop-A16":   "laptop-a16",
		" mac 14 / x ": "mac-14---x",
		"---":          "",
		"":             "",
		"a_b.c":        "a_b.c",
	} {
		if got := cleanLabel(in); got != want {
			t.Errorf("cleanLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeMac makes this process look like the given Mac, with a temp HOME.
func fakeMac(t *testing.T, model, chip, profName, profChip string, profOK bool) (profileCalls *int) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_HOST", "")
	oldM, oldC, oldP := probeModel, probeChip, probeProfile
	t.Cleanup(func() { probeModel, probeChip, probeProfile = oldM, oldC, oldP })
	calls := 0
	probeModel = func() string { return model }
	probeChip = func() string { return chip }
	probeProfile = func() (string, string, bool) { calls++; return profName, profChip, profOK }
	return &calls
}

func TestLabelOnAMacAddsChipAndProduct(t *testing.T) {
	fakeMac(t, "MacBookPro18,3", "Apple M1 Pro", "MacBook Pro", "Apple M1 Pro", true)
	if got, want := Label(), Host()+"-m1pro-mbp"; got != want {
		t.Fatalf("Label() = %q, want %q", got, want)
	}
}

func TestLabelAsksTheProfilerOnceAndCachesPerModel(t *testing.T) {
	calls := fakeMac(t, "MacBookPro18,3", "Apple M1 Pro", "MacBook Pro", "Apple M1 Pro", true)
	first := Label()
	if Label() != first || Label() != first {
		t.Fatal("label changed between calls")
	}
	if *calls != 1 {
		t.Fatalf("system_profiler asked %d times, want 1", *calls)
	}
	if b, _ := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".config/aivoicebus/hostlabel.json")); !strings.Contains(string(b), "m1pro-mbp") {
		t.Fatalf("cache not written: %q", b)
	}
	// a different model invalidates the cache
	probeModel = func() string { return "Mac16,10" }
	probeProfile = func() (string, string, bool) { *calls++; return "Mac mini", "Apple M4", true }
	if got := Label(); got != Host()+"-m4-mini" {
		t.Fatalf("new model not picked up: %q", got)
	}
	if *calls != 2 {
		t.Fatalf("profiler calls = %d, want 2", *calls)
	}
}

func TestLabelFallsBackWithoutCachingWhenProfilerFails(t *testing.T) {
	calls := fakeMac(t, "MacBookPro18,3", "Apple M1 Pro", "", "", false)
	if got, want := Label(), Host()+"-m1pro-mbp"; got != want { // legacy identifier still says "MacBookPro"
		t.Fatalf("fallback label = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".config/aivoicebus/hostlabel.json")); err == nil {
		t.Fatal("a fallback answer must not be cached")
	}
	Label()
	if *calls != 2 {
		t.Fatalf("profiler should be retried while it fails, calls = %d", *calls)
	}
}

func TestLabelOverrideAndNonMac(t *testing.T) {
	fakeMac(t, "MacBookPro18,3", "Apple M1 Pro", "MacBook Pro", "Apple M1 Pro", true)
	t.Setenv("AIVOICEBUS_HOST", "Laptop M1Pro MBP16/x")
	if got := Label(); strings.ContainsAny(got, " /") || !strings.HasPrefix(got, "laptop") {
		t.Fatalf("override not applied safely: %q", got)
	}
	t.Setenv("AIVOICEBUS_HOST", "")
	probeModel = func() string { return "" } // a server
	if got := Label(); got != Host() {
		t.Fatalf("non-Mac label = %q, want the plain host %q", got, Host())
	}
}

func TestRepoFromURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:aivoicebus/aivoicebus.git":      "aivoicebus",
		"https://github.com/example-org/Go-URL-Api.git": "go-url-api",
		"https://github.com/baditaflorin/aivoicebus/":   "aivoicebus",
		"/srv/git/some.repo.git":                        "some.repo",
		"ssh://git@host:2222/team/name":                 "name",
		"name":                                          "name",
		"":                                              "",
		"https://example.com/a/ünï":                     "n",
	} {
		if got := repoFromURL(in); got != want {
			t.Errorf("repoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLabelsFollowTheIdentityAndTheEnvironment(t *testing.T) {
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "Laptop-A/Claude.A1B2")
	l := Labels(t.TempDir()) // not a git checkout
	if l["machine"] != "laptop-a" || l["tool"] != "claude" {
		t.Errorf("machine/tool come from the identity: %v", l)
	}
	for _, k := range []string{"host", "chip", "product", "repo", "branch"} {
		if _, ok := l[k]; ok {
			t.Errorf("an overridden identity in a non-repo directory must not claim %q: %v", k, l)
		}
	}
	// the environment is laid over what was derived
	t.Setenv("AIVOICEBUS_LABELS", "repo=Pinned-Repo,role=reviewer,tool=codex")
	l = Labels(t.TempDir())
	if l["repo"] != "pinned-repo" || l["role"] != "reviewer" || l["tool"] != "codex" || l["machine"] != "laptop-a" {
		t.Errorf("env overlay: %v", l)
	}
}

func TestLabelsOnAMacIncludeHostChipAndProduct(t *testing.T) {
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "")
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1b2")
	fakeMac(t, "MacBookPro18,3", "Apple M1 Pro", "MacBook Pro", "Apple M1 Pro", true)
	l := Labels(t.TempDir())
	if l["host"] != Host() || l["chip"] != "m1pro" || l["product"] != "mbp" || l["machine"] != Host()+"-m1pro-mbp" || l["tool"] != "claude" {
		t.Errorf("%v", l)
	}
}

// gitRepo makes a real repository whose origin and branch are given.
func gitRepo(t *testing.T, origin, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	run := func(args ...string) {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("symbolic-ref", "HEAD", "refs/heads/"+branch)
	if origin != "" {
		run("remote", "add", "origin", origin)
	}
	return dir
}

func TestLabelsReadRepoAndBranchFromGit(t *testing.T) {
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "x/claude")
	dir := gitRepo(t, "git@github.com:example-org/Go-URL-Categorizer-Api.git", "feature/AiVoiceBus")
	sub := filepath.Join(dir, "deep", "er")
	os.MkdirAll(sub, 0o755)
	for _, where := range []string{dir, sub} { // anywhere inside the checkout
		l := Labels(where)
		if l["repo"] != "go-url-categorizer-api" || l["branch"] != "feature/aivoicebus" {
			t.Errorf("%s: %v", where, l)
		}
	}
}

func TestLabelsWithoutAnOriginUseTheDirectoryNameAndDetachedHeadHasNoBranch(t *testing.T) {
	t.Setenv("AIVOICEBUS_LABELS", "")
	t.Setenv("AIVOICEBUS_ME", "x/claude")
	dir := gitRepo(t, "", "main")
	l := Labels(dir)
	if l["repo"] != msgClean(filepath.Base(dir)) || l["branch"] != "main" {
		t.Errorf("no origin: %v", l)
	}
	// detached HEAD: a commit hash instead of a ref
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("0123456789012345678901234567890123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if l = Labels(dir); l["repo"] == "" || l["branch"] != "" {
		t.Errorf("detached HEAD keeps the repo but has no branch: %v", l)
	}
}

func msgClean(s string) string { return msg.CleanValue(s) }

func TestOwnerRepoFromURLNeedsAnOwnerAndAHost(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:AiVoiceBus/aivoicebus.git":    "aivoicebus/aivoicebus",
		"https://github.com/baditaflorin/aivoicebus/": "baditaflorin/aivoicebus",
		"ssh://git@host:2222/team/sub/name.git":       "sub/name",
		"/srv/git/name.git":                           "", // a path on disk has no owner
		"https://github.com/name":                     "",
		"":                                            "",
	} {
		if got := ownerRepoFromURL(in); got != want {
			t.Errorf("ownerRepoFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
