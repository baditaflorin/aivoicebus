package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/credstore"
)

// Built from pieces so no scanner takes it for a real credential.
var secTok = "tok" + "-" + "abcDEF123456" + "_x.y~z"

// secretEnv makes a home with nothing in it and a server that knows secTok. Checks may go to
// a local address in these tests (and only in tests: see allowLoopbackCheck).
func secretEnv(t *testing.T) (credDir string, srv *httptest.Server) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_ALLOW_LOCAL_CHECK", "1")
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secTok {
			http.Error(w, "no", 401)
			return
		}
		w.Write([]byte(`{"login":"alice"}`))
	}))
	t.Cleanup(srv.Close)
	return filepath.Join(home, ".config", "aivoicebus", "credentials"), srv
}

// hostOnly is the host of a test server URL without its port: what the command announces.
func hostOnly(u string) string {
	h := strings.TrimPrefix(u, "http://")
	return h[:strings.LastIndex(h, ":")]
}

func runSecret(t *testing.T, in string, args ...string) (code int, out, errs string) {
	t.Helper()
	var o, e strings.Builder
	code = Run(append([]string{"secret"}, args...), Streams{In: strings.NewReader(in), Out: &o, Err: &e})
	if strings.Contains(o.String(), secTok) || strings.Contains(e.String(), secTok) {
		t.Fatalf("the credential was printed: out=%q err=%q", o.String(), e.String())
	}
	return code, o.String(), e.String()
}

func TestSecretSaveFromAPipeChecksThenSaves(t *testing.T) {
	dir, srv := secretEnv(t)
	code, out, errs := runSecret(t, secTok+"\n", "save", "svc", "--check-url", srv.URL+"/user")
	if code != 0 || !strings.Contains(out, `valid for "alice"`) {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
	p := filepath.Join(dir, "svc", "token")
	if b, err := os.ReadFile(p); err != nil || string(b) != secTok {
		t.Fatalf("the file: %q %v", b, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %o", fi.Mode().Perm())
	}
	if !strings.Contains(out, p) {
		t.Errorf("the path is not reported: %q", out)
	}
	// the host the check went to is the host the credential may be sent to
	if hs, _ := credstore.Hosts(dir, "svc"); len(hs) != 1 || !strings.Contains(srv.URL, hs[0]) || !strings.Contains(out, "may be sent to: "+hs[0]) {
		t.Errorf("hosts %v, out %q", hs, out)
	}
	// where the token is sent is said before it is read, and a custom address is called out
	if want := "sent once, to " + hostOnly(srv.URL) + " (an address"; !strings.Contains(errs, want) || !strings.Contains(errs, "make sure you trust it") {
		t.Errorf("the destination is not announced: %q", errs)
	}
}

func TestSecretSaveAnnouncesTheBuiltInCheckHostAndNothingWhenUnchecked(t *testing.T) {
	_, srv := secretEnv(t)
	old, had := credstore.Presets["deploykey"]
	credstore.Presets["deploykey"] = credstore.Preset{URL: srv.URL + "/user", Field: "login"} // no real network in a test
	defer func() {
		if had {
			credstore.Presets["deploykey"] = old
		} else {
			delete(credstore.Presets, "deploykey")
		}
	}()
	_, _, errs := runSecret(t, secTok, "save", "deploykey")
	if want := "sent once, to " + hostOnly(srv.URL) + " (built-in check)"; !strings.Contains(errs, want) {
		t.Errorf("the built-in destination is not announced: %q", errs)
	}
	if _, _, errs := runSecret(t, secTok, "save", "deploykey", "--no-check"); strings.Contains(errs, "sent once") {
		t.Errorf("nothing is sent with --no-check, so nothing is announced: %q", errs)
	}
}

func TestSecretSaveRefusesALocalCheckAddressUnlessTesting(t *testing.T) {
	_, srv := secretEnv(t)
	t.Setenv("AIVOICEBUS_ALLOW_LOCAL_CHECK", "")
	code, _, errs := runSecret(t, secTok, "save", "svc", "--check-url", srv.URL+"/user")
	if code == 0 || !strings.Contains(errs, "https") {
		t.Errorf("a check to http://localhost must be refused outside tests: %d %q", code, errs)
	}
}

func TestSecretSaveRefusesATokenTheServiceRejects(t *testing.T) {
	dir, srv := secretEnv(t)
	code, _, errs := runSecret(t, "wrong-"+secTok+"\n", "save", "svc", "--check-url", srv.URL+"/user")
	if code != 1 || !strings.Contains(errs, "rejected the token") || !strings.Contains(errs, "nothing was saved") {
		t.Fatalf("code %d err %q", code, errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "svc")); err == nil {
		t.Error("a rejected token was saved")
	}
	// --no-check saves it anyway, on purpose
	if code, out, _ := runSecret(t, "wrong-"+secTok, "save", "svc", "--no-check"); code != 0 || !strings.Contains(out, "unchecked") {
		t.Errorf("--no-check: %d %q", code, out)
	}
}

func TestSecretSaveWithNoCheckNeedsAHostForCurl(t *testing.T) {
	dir, _ := secretEnv(t)
	code, out, errs := runSecret(t, secTok, "save", "hetzner")
	if code != 0 || !strings.Contains(out, "was not checked") || !strings.Contains(out, "not bound to any host") {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
	if code, out, _ := runSecret(t, secTok, "save", "hetzner", "--host", "API.Hetzner.cloud,dns.hetzner.com"); code != 0 || !strings.Contains(out, "may be sent to: api.hetzner.cloud, dns.hetzner.com") {
		t.Errorf("--host: %d %q", code, out)
	}
	if hs, _ := credstore.Hosts(dir, "hetzner"); len(hs) != 2 {
		t.Errorf("%v", hs)
	}
	if code, _, _ := runSecret(t, secTok, "save", "x", "--host", "https://a.com"); code == 0 {
		t.Error("a host with a scheme must be refused")
	}
}

func TestSecretSaveRefusesBadNamesAndPastes(t *testing.T) {
	dir, _ := secretEnv(t)
	for _, c := range []struct{ in, name string }{{secTok, "aivoicebus"}, {secTok, "../etc"}, {secTok, "Bad Name"}, {"two words", "ok"}, {"", "ok"}, {"a\nb", "ok"}} {
		if code, _, _ := runSecret(t, c.in, "save", c.name, "--no-check"); code == 0 {
			t.Errorf("save %q with %q must fail", c.name, c.in)
		}
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Errorf("refusals left files: %v", es)
	}
	if code, _, errs := runSecret(t, secTok, "save"); code == 0 || !strings.Contains(errs, "usage") {
		t.Errorf("no name: %d %q", code, errs)
	}
}

func TestSecretListPathRm(t *testing.T) {
	dir, _ := secretEnv(t)
	if code, out, _ := runSecret(t, "", "list"); code != 0 || !strings.Contains(out, "no credentials") {
		t.Errorf("empty list: %d %q", code, out)
	}
	runSecret(t, secTok, "save", "beta", "--no-check")
	runSecret(t, secTok, "save", "alpha", "--no-check", "--host", "a.example.com")
	code, out, _ := runSecret(t, "", "list")
	if code != 0 || strings.Index(out, "alpha") > strings.Index(out, "beta") || !strings.Contains(out, "0600") || !strings.Contains(out, "a.example.com") || !strings.Contains(out, "no host") {
		t.Errorf("list: %d %q", code, out)
	}
	if code, out, _ := runSecret(t, "", "path", "alpha"); code != 0 || strings.TrimSpace(out) != filepath.Join(dir, "alpha", "token") {
		t.Errorf("path: %d %q", code, out)
	}
	if code, _, _ := runSecret(t, "", "path", "nope"); code == 0 {
		t.Error("path of a missing credential must fail")
	}
	os.Chmod(filepath.Join(dir, "alpha", "token"), 0o644)
	if _, out, _ := runSecret(t, "", "list"); !strings.Contains(out, "readable by others") {
		t.Errorf("a loose file is flagged: %q", out)
	}
	if code, _, _ := runSecret(t, "", "path", "alpha"); code == 0 {
		t.Error("a loose file must not be handed out")
	}
	if code, out, _ := runSecret(t, "", "rm", "beta"); code != 0 || !strings.Contains(out, "removed beta") {
		t.Errorf("rm: %d %q", code, out)
	}
	if code, _, _ := runSecret(t, "", "rm", "beta"); code == 0 {
		t.Error("removing twice must fail")
	}
}

// Other tools keep their own files under ~/.config/<name>/; credentials never collide with them.
func TestSecretNeverTouchesAnotherToolsConfig(t *testing.T) {
	dir, _ := secretEnv(t)
	other := filepath.Join(filepath.Dir(filepath.Dir(dir)), "gh", "token") // ~/.config/gh/token
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("not-ours"), 0o644)
	if code, _, _ := runSecret(t, secTok, "save", "gh", "--no-check"); code != 0 {
		t.Fatal("save gh")
	}
	if b, _ := os.ReadFile(other); string(b) != "not-ours" {
		t.Errorf("another tool's file was changed: %q", b)
	}
	if fi, _ := os.Stat(filepath.Dir(other)); fi.Mode().Perm() != 0o755 {
		t.Errorf("another tool's directory was chmodded: %o", fi.Mode().Perm())
	}
	runSecret(t, "", "rm", "gh")
	if _, err := os.Stat(other); err != nil {
		t.Errorf("rm touched another tool's file: %v", err)
	}
}

// fakeCurl puts a curl in PATH that records what it was called with and what came in on stdin,
// then behaves as script says (the body of a /bin/sh script after the recording; $IN is the file
// holding what came in on stdin).
func fakeCurl(t *testing.T, script string) (argv, stdin string) {
	t.Helper()
	bin, rec := t.TempDir(), t.TempDir()
	argv, stdin = filepath.Join(rec, "argv"), filepath.Join(rec, "stdin")
	body := "#!/bin/sh\nIN='" + stdin + "'\nprintf '%s\\n' \"$@\" > '" + argv + "'\ncat > \"$IN\"\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argv, stdin
}

// The credential must reach curl on stdin, in a config: an argument is visible to every
// process on the machine (ps), and ~/.curlrc must not be read (-q first).
func TestSecretCurlHandsTheTokenToCurlOnStdinNotInArguments(t *testing.T) {
	secretEnv(t)
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	argv, stdin := fakeCurl(t, "")
	if code, _, errs := runSecret(t, "", "curl", "svc", "--", "-sS", "https://api.example.test/x"); code != 0 {
		t.Fatalf("%d %q", code, errs)
	}
	gotArgv, _ := os.ReadFile(argv)
	gotStdin, _ := os.ReadFile(stdin)
	if strings.Contains(string(gotArgv), secTok) {
		t.Errorf("the token is in curl's arguments:\n%s", gotArgv)
	}
	if want := "-q\n-K\n-\n--proto\n=https\n-sS\nhttps://api.example.test/x\n"; string(gotArgv) != want {
		t.Errorf("curl was called with %q, want %q", gotArgv, want)
	}
	if string(gotStdin) != "header = \"Authorization: Bearer "+secTok+"\"\n" {
		t.Errorf("curl's config on stdin: %q", gotStdin)
	}
	// another header style, and flags before the name
	fakeCurl(t, "")
	if code, _, errs := runSecret(t, "", "curl", "--header", "X-Api-Key: {}", "svc", "--", "https://api.example.test/y"); code != 0 {
		t.Fatalf("custom header: %d %q", code, errs)
	}
}

// -v makes curl print the request headers, which hold the credential: that is how an agent
// debugging a 401 would put it into the transcript. Refused, and if curl echoes it anyway, masked.
func TestSecretCurlRefusesVerboseAndMasksAnythingCurlEchoes(t *testing.T) {
	secretEnv(t)
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	argv, _ := fakeCurl(t, "")
	for _, bad := range [][]string{{"-v"}, {"-sSv"}, {"--verbose"}, {"--trace-ascii", "-"}, {"--libcurl", "-"}, {"-L"}, {"--location-trusted"}, {"-K", "x"}, {"--next"}} {
		code, _, errs := runSecret(t, "", append(append([]string{"curl", "svc", "--"}, bad...), "https://api.example.test/x")...)
		if code == 0 || !strings.Contains(errs, "not allowed") {
			t.Errorf("%v: %d %q", bad, code, errs)
		}
	}
	if _, err := os.Stat(argv); err == nil {
		t.Error("curl was run despite a refused option")
	}
}

// If curl ever echoes the header (a debug build, an error page that reflects it), it is masked.
func TestSecretCurlMasksTheTokenInBothStreams(t *testing.T) {
	secretEnv(t)
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	fakeCurl(t, `cat "$IN"; cat "$IN" >&2`)
	code, out, errs := runSecret(t, "", "curl", "svc", "--", "https://api.example.test/x")
	if code != 0 || !strings.Contains(out, "Bearer <credential>") || !strings.Contains(errs, "Bearer <credential>") {
		t.Errorf("the echoed header is masked in both streams: %d out %q err %q", code, out, errs)
	}
}

func TestSecretCurlRefusesOtherHostsUnboundCredentialsAndMissingOnes(t *testing.T) {
	secretEnv(t)
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	runSecret(t, secTok, "save", "unbound", "--no-check")
	argv, _ := fakeCurl(t, "")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"curl", "svc", "--", "https://evil.example.com/x"}, "may only be sent to api.example.test"},
		{[]string{"curl", "svc", "--", "https://api.example.test/x", "https://evil.example.com/y"}, "may only be sent to"},
		{[]string{"curl", "svc", "--", "http://api.example.test/x"}, "https"},
		{[]string{"curl", "svc", "--", "-sS"}, "full https URL"},
		{[]string{"curl", "unbound", "--", "https://api.example.test/x"}, "--host HOST"},
		{[]string{"curl", "nope", "--", "https://api.example.test/x"}, "aivoicebus secret save nope"},
		{[]string{"curl", "svc", "--header", "no placeholder", "--", "https://api.example.test/x"}, "{}"},
	} {
		if code, _, errs := runSecret(t, "", c.args...); code == 0 || !strings.Contains(errs, c.want) {
			t.Errorf("%v: %d %q (want %q)", c.args, code, errs, c.want)
		}
	}
	if _, err := os.Stat(argv); err == nil {
		t.Error("curl was run for a refused request")
	}
}

func TestSecretCurlPassesCurlsExitCodeOn(t *testing.T) {
	secretEnv(t)
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	fakeCurl(t, "exit 22")
	if code, _, _ := runSecret(t, "", "curl", "svc", "--", "https://api.example.test/x"); code != 22 {
		t.Errorf("exit code %d, want curl's 22", code)
	}
}

func TestSecretUsage(t *testing.T) {
	secretEnv(t)
	if code, _, errs := runSecret(t, ""); code != 2 || !strings.Contains(errs, "aivoicebus secret save NAME") {
		t.Errorf("bare: %d %q", code, errs)
	}
	if code, _, _ := runSecret(t, "", "help"); code != 0 {
		t.Errorf("help: %d", code)
	}
	if code, _, _ := runSecret(t, "", "frobnicate"); code != 2 {
		t.Errorf("unknown subcommand: %d", code)
	}
}

// A credential handed over with `secret save` is a secret like the mesh token: it cannot be put
// into a message, even one a hostile text talked an agent into writing.
func TestASavedCredentialCannotBeSentInAMessage(t *testing.T) {
	secretEnv(t)
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // nothing listens: the refusal must come before any request
	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_ME", "air/codex")
	runSecret(t, secTok, "save", "svc", "--no-check", "--host", "api.example.test")
	var o, e strings.Builder
	code := Run([]string{"send", "--to", "air/codex", "the token is " + secTok}, Streams{In: strings.NewReader(""), Out: &o, Err: &e})
	if code == 0 || strings.Contains(o.String()+e.String(), secTok) || !strings.Contains(e.String(), "secret from this machine") {
		t.Errorf("a saved credential must be refused without being echoed: code %d out %q err %q", code, o.String(), e.String())
	}
}
