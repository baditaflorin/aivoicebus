package credstore

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The test credentials are built from pieces so no scanner takes them for real ones.
var tok = "tok" + "-" + "abcDEF123456" + "_x.y~z"

func TestValidName(t *testing.T) {
	for _, ok := range []string{"deploykey", "github", "hetzner-dns", "a", "x_y-9"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("%q should be a name: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "CI", "9lives", "-x", "a/b", "../etc", "a b", strings.Repeat("a", 41), "aivoicebus", "a.b"} {
		if ValidName(bad) == nil {
			t.Errorf("%q must not be a name", bad)
		}
	}
}

func TestValidHostRefusesLookAlikesAndPorts(t *testing.T) {
	for _, ok := range []string{"ci.example.com", "api.github.com", "localhost", "a"} {
		if !ValidHost(ok) {
			t.Errorf("%q is a host", ok)
		}
	}
	for _, bad := range []string{"", "CI.example.com", "ci.exaеc.com", "a b", "a/b", "ci.example.com:443", "-a.com", "a.com.", "https://a.com"} {
		if ValidHost(bad) {
			t.Errorf("%q is not a host", bad)
		}
	}
}

func TestCleanTrimsPastesAndRefusesWhatNoTokenHolds(t *testing.T) {
	if got, err := Clean("  " + tok + "\r\n"); err != nil || got != tok {
		t.Errorf("a paste with its line break: %q %v", got, err)
	}
	for name, bad := range map[string]string{
		"empty": "", "blank": " \n ", "inner space": "ab cd", "inner newline": "ab\ncd", "quote": `ab"cd`, "backslash": `ab\cd`,
		"non-ascii": "abécd", "control": "ab\x01cd", "too long": strings.Repeat("a", MaxLen+1),
	} {
		got, err := Clean(bad)
		if err == nil {
			t.Errorf("%s: accepted as %q", name, got)
		} else if bad != "" && strings.Contains(err.Error(), strings.TrimSpace(bad)) && len(strings.TrimSpace(bad)) > 3 {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
	// what real tokens look like
	for _, real := range []string{"ghp_" + strings.Repeat("a1B2", 9), "github_pat_" + "11ABC_" + strings.Repeat("x", 40), "eyJhbGciOi.eyJzdWIi.c2ln-_", "dGVzdA+/=="} {
		if _, err := Clean(real); err != nil {
			t.Errorf("a real-looking token %q refused: %v", real, err)
		}
	}
}

func TestSaveLoadRoundTripWithPrivateModesAndHosts(t *testing.T) {
	base := filepath.Join(t.TempDir(), "credentials") // base does not exist yet
	if err := Save(base, "deploykey", " "+tok+"\n", []string{"CI.example.com", "ci.example.com", " api.github.com"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		p    string
		mode os.FileMode
	}{{Path(base, "deploykey"), 0o600}, {filepath.Join(base, "deploykey", HostsFile), 0o600}, {filepath.Join(base, "deploykey"), 0o700}, {base, 0o700}} {
		if fi, err := os.Stat(c.p); err != nil || fi.Mode().Perm() != c.mode {
			t.Errorf("%s: %v %v, want %o", c.p, fi, err, c.mode)
		}
	}
	if got, err := Load(base, "deploykey"); err != nil || got != tok {
		t.Errorf("load: %q %v", got, err)
	}
	if hs, err := Hosts(base, "deploykey"); err != nil || strings.Join(hs, ",") != "api.github.com,ci.example.com" {
		t.Errorf("hosts (lower-cased, deduplicated, sorted): %v %v", hs, err)
	}
	// overwriting replaces both, and leaves no temporary file behind
	if err := Save(base, "deploykey", tok+"2", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(base, "deploykey"); got != tok+"2" {
		t.Errorf("overwrite: %q", got)
	}
	if hs, _ := Hosts(base, "deploykey"); len(hs) != 0 {
		t.Errorf("a save without hosts forgets the old ones: %v", hs)
	}
	if ents, _ := os.ReadDir(filepath.Join(base, "deploykey")); len(ents) != 2 {
		t.Errorf("leftovers: %v", ents)
	}
}

func TestSaveTightensALooseDirectoryAndRefusesBadInput(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "github")
	os.MkdirAll(dir, 0o755)
	os.Chmod(dir, 0o755)
	if err := Save(base, "github", tok, nil); err != nil {
		t.Fatal(err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("the directory stayed %o", di.Mode().Perm())
	}
	for _, c := range []struct {
		name, tok string
		hosts     []string
	}{{"aivoicebus", tok, nil}, {"../x", tok, nil}, {"ok", "has space", nil}, {"ok", "", nil}, {"ok", tok, []string{"ci.exaеc.com"}}, {"ok", tok, []string{"https://a.com"}}} {
		if Save(base, c.name, c.tok, c.hosts) == nil {
			t.Errorf("Save(%q, %q, %v) must fail", c.name, c.tok, c.hosts)
		}
	}
	if _, err := os.Stat(filepath.Join(base, "ok")); err == nil {
		t.Error("a refused credential left a directory")
	}
}

// A dotfile manager (stow and friends) often makes ~/.config/... a symlink into a repository.
func TestSaveAndRemoveNeverFollowOrDeleteASymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(base, "github")); err != nil {
		t.Fatal(err)
	}
	if err := Save(base, "github", tok, nil); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("save behind a symlink: %v", err)
	}
	if ents, _ := os.ReadDir(elsewhere); len(ents) != 0 {
		t.Errorf("the token landed behind the symlink: %v", ents)
	}
	// somebody's own file behind the link must survive a refused remove
	theirs := filepath.Join(elsewhere, File)
	os.WriteFile(theirs, []byte("theirs"), 0o600)
	if err := Remove(base, "github"); err == nil {
		t.Error("remove must refuse a symlinked directory")
	}
	if b, err := os.ReadFile(theirs); err != nil || string(b) != "theirs" {
		t.Errorf("a file behind the symlink was deleted or changed: %q %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(base, "github")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the user's symlink was deleted: %v", err)
	}
}

func TestLoadRefusesAFileOthersCanReadAndAnythingSpecial(t *testing.T) {
	base := t.TempDir()
	Save(base, "svc", tok, nil)
	os.Chmod(Path(base, "svc"), 0o644)
	if _, err := Load(base, "svc"); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), tok) {
		t.Errorf("a world-readable credential must be refused with the fix named, not the value: %v", err)
	}
	os.Chmod(Path(base, "svc"), 0o600)
	if _, err := Load(base, "svc"); err != nil {
		t.Error(err)
	}
	if _, err := Load(base, "missing"); err == nil || !strings.Contains(err.Error(), "aivoicebus secret save missing") {
		t.Errorf("a missing credential says how to save it: %v", err)
	}
	// a directory where the file should be is not read (and does not block)
	os.MkdirAll(filepath.Join(base, "dirtok", File), 0o700)
	if _, err := Load(base, "dirtok"); err == nil {
		t.Error("a directory is not a credential")
	}
}

func TestListShowsNamesModesAndHostsNeverValues(t *testing.T) {
	base := t.TempDir()
	Save(base, "zeta", tok, []string{"z.example.com"})
	Save(base, "alpha", tok, nil)
	os.MkdirAll(filepath.Join(base, "aivoicebus"), 0o700) // reserved
	os.WriteFile(filepath.Join(base, "aivoicebus", File), []byte("mesh"), 0o600)
	os.MkdirAll(filepath.Join(base, "Not A Name"), 0o700)
	os.MkdirAll(filepath.Join(base, "empty"), 0o700)
	es, err := List(base)
	if err != nil || len(es) != 2 || es[0].Name != "alpha" || es[1].Name != "zeta" {
		t.Fatalf("%+v %v", es, err)
	}
	if !es[0].Private || es[0].Mode != 0o600 || es[0].Size != int64(len(tok)) || len(es[0].Hosts) != 0 || strings.Join(es[1].Hosts, ",") != "z.example.com" {
		t.Errorf("%+v %+v", es[0], es[1])
	}
	if es, err := List(filepath.Join(base, "nothing-here")); err != nil || len(es) != 0 {
		t.Errorf("a missing base is an empty list: %v %v", es, err)
	}
}

func TestRemove(t *testing.T) {
	base := t.TempDir()
	Save(base, "svc", tok, []string{"a.example.com"})
	if err := Remove(base, "svc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "svc")); err == nil {
		t.Error("the directory stayed")
	}
	if Remove(base, "svc") == nil || Remove(base, "aivoicebus") == nil {
		t.Error("removing what is not there, or a reserved name, must fail")
	}
}

func ownerServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/redirect":
			http.Redirect(w, r, "https://elsewhere.invalid/", http.StatusFound)
		case r.URL.Path == "/html":
			w.Write([]byte("<html>login</html>"))
		case r.URL.Path == "/nofield":
			w.Write([]byte(`{"name":"x"}`))
		case r.URL.Path == "/long":
			w.Write([]byte(`{"login":"` + strings.Repeat("a", 500) + `"}`))
		case r.URL.Path == "/server-error":
			http.Error(w, "boom", 500)
		case r.Header.Get("Authorization") != "Bearer "+tok:
			http.Error(w, "no", 401)
		default:
			w.Write([]byte(`{"login":"alice","id":7}`))
		}
	}))
}

func TestCheckNamesTheOwnerOrSaysWhyNot(t *testing.T) {
	srv := ownerServer(t)
	defer srv.Close()
	ctx := context.Background()
	if got, err := Check(ctx, srv.Client(), srv.URL+"/user", "login", tok, true); err != nil || got != "alice" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := Check(ctx, srv.Client(), srv.URL+"/long", "login", tok, true); len(got) > 70 {
		t.Errorf("a hostile owner name is cut: %d characters", len(got))
	}
	for name, c := range map[string]struct{ path, want string }{
		"wrong token": {"/user", "rejected the token"},
		"redirect":    {"/redirect", "HTTP 302"},
		"html":        {"/html", "not answer with JSON"},
		"no field":    {"/nofield", `no "login" field`},
		"server":      {"/server-error", "HTTP 500"},
	} {
		tk := tok
		if name == "wrong token" {
			tk = "wrong" + tok
		}
		_, err := Check(ctx, srv.Client(), srv.URL+c.path, "login", tk, true)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		} else if strings.Contains(err.Error(), tk) {
			t.Errorf("%s: the error holds the token", name)
		}
	}
	// a loopback address is refused unless the caller (a test) says it is fine
	if _, err := Check(ctx, srv.Client(), srv.URL+"/user", "login", tok, false); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("loopback http must be refused by default: %v", err)
	}
	// refused before any request is made, for the right reason (not because the network failed)
	for bad, want := range map[string]string{"http://example.com/user": "must be https", "ftp://x/y": "must be https", "not a url": "not a URL", "": "not a URL", "https://ci.exaеc.com/user": "look-alike"} {
		if _, err := Check(ctx, srv.Client(), bad, "login", tok, true); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v (want %q)", bad, err, want)
		}
	}
}

func TestCurlConfigPutsTheTokenInAHeaderOnly(t *testing.T) {
	got, err := CurlConfig("Authorization: Bearer {}", tok)
	if err != nil || got != "header = \"Authorization: Bearer "+tok+"\"\n" {
		t.Errorf("%q %v", got, err)
	}
	if got, err := CurlConfig("X-Api-Key: {}", tok); err != nil || !strings.Contains(got, "X-Api-Key: "+tok) {
		t.Errorf("%q %v", got, err)
	}
	for _, bad := range []string{"Authorization: Bearer", "A: {} {}", "A: {}\"; url = \"http://evil", "A: {}\nB: c", `A: {}\`} {
		if _, err := CurlConfig(bad, tok); err == nil {
			t.Errorf("header %q must be refused", bad)
		}
	}
	if _, err := CurlConfig("A: {}", "has space"); err == nil {
		t.Error("a credential with a space must be refused")
	}
}

func TestCheckCurlArgs(t *testing.T) {
	hosts := []string{"ci.example.com"}
	ok := [][]string{
		{"https://ci.example.com/api/user"},
		{"-sS", "https://ci.example.com/api/user", "-w", "%{http_code}"},
		{"-X", "POST", "-H", "Content-Type: application/json", "-d", `{"branch":"main"}`, "https://ci.example.com/api/repos/1/pipelines"},
		{"--fail-with-body", "--max-time", "20", "--retry=2", "-o", "out.json", "https://CI.example.com:443/x"},
		{"--tr-encoding", "https://ci.example.com/"}, // starts like --trace but is another option
	}
	for _, a := range ok {
		if _, err := CheckCurlArgs(a, hosts); err != nil {
			t.Errorf("%v should be allowed: %v", a, err)
		}
	}
	bad := map[string][]string{
		"verbose":              {"-v", "https://ci.example.com/x"},
		"verbose in a cluster": {"-sSv", "https://ci.example.com/x"},
		"long verbose":         {"--verbose", "https://ci.example.com/x"},
		"abbreviated verbose":  {"--verb", "https://ci.example.com/x"},
		"trace":                {"--trace", "-", "https://ci.example.com/x"},
		"trace-ascii":          {"--trace-ascii=-", "https://ci.example.com/x"},
		"libcurl":              {"--libcurl", "-", "https://ci.example.com/x"},
		"config":               {"-K", "evil.cfg", "https://ci.example.com/x"},
		"long config":          {"--config=evil.cfg", "https://ci.example.com/x"},
		"follow redirects":     {"-L", "https://ci.example.com/x"},
		"cluster with L":       {"-sSL", "https://ci.example.com/x"},
		"location-trusted":     {"--location-trusted", "https://ci.example.com/x"},
		"next":                 {"https://ci.example.com/x", "--next", "https://ci.example.com/y"},
		"url option":           {"--url", "https://ci.example.com/x"},
		"proxy":                {"-x", "http://evil:8080", "https://ci.example.com/x"},
		"resolve":              {"--resolve", "ci.example.com:443:127.0.0.1", "https://ci.example.com/x"},
		"other host":           {"https://evil.example.com/x"},
		"two hosts":            {"https://ci.example.com/x", "https://evil.example.com/y"},
		"plain http":           {"http://ci.example.com/x"},
		"no url":               {"-sS"},
		"look-alike host":      {"https://ci.exaеc.com/x"},
	}
	for name, a := range bad {
		if _, err := CheckCurlArgs(a, hosts); err == nil {
			t.Errorf("%s: %v must be refused", name, a)
		}
	}
	if _, err := CheckCurlArgs([]string{"https://ci.example.com/x"}, nil); err == nil || !strings.Contains(err.Error(), "--host") {
		t.Errorf("a credential bound to no host is refused, and says how to fix it: %v", err)
	}
}

func TestRedactorMasksTheSecretEvenSplitAcrossWrites(t *testing.T) {
	const secret = "s3cr3t-value-0123456789"
	for name, parts := range map[string][]string{
		"whole":           {"before " + secret + " after"},
		"twice":           {secret + secret},
		"split in two":    {"x " + secret[:7], secret[7:] + " y"},
		"split in many":   {"a", secret[:3], secret[3:9], secret[9:15], secret[15:], "b"},
		"a false start":   {"s3cr3t-valu", "e is not it, but " + secret},
		"byte by byte":    {strings.Join(strings.Split("pre "+secret+" post", ""), "\x00")},
		"nothing to mask": {"hello ", "world"},
		"ends mid-secret": {"tail " + secret[:10]},
	} {
		var out bytes.Buffer
		r := Redactor(&out, secret)
		in := ""
		if name == "byte by byte" {
			in = "pre " + secret + " post"
			for i := 0; i < len(in); i++ {
				r.Write([]byte{in[i]})
			}
		} else {
			for _, p := range parts {
				in += p
				if n, err := r.Write([]byte(p)); err != nil || n != len(p) {
					t.Fatalf("%s: write %d %v", name, n, err)
				}
			}
		}
		r.Close()
		if strings.Contains(out.String(), secret) {
			t.Errorf("%s: the secret got through: %q", name, out.String())
		}
		if want := strings.ReplaceAll(in, secret, "<credential>"); out.String() != want {
			t.Errorf("%s: got %q want %q", name, out.String(), want)
		}
	}
	// an empty secret masks nothing and does not loop
	var out bytes.Buffer
	r := Redactor(&out, "")
	r.Write([]byte("abc"))
	r.Close()
	if out.String() != "abc" {
		t.Errorf("%q", out.String())
	}
}

func TestPresetsAreHTTPSAndNamed(t *testing.T) {
	for name, p := range Presets {
		if ValidName(name) != nil || !strings.HasPrefix(p.URL, "https://") || p.Field == "" {
			t.Errorf("preset %q: %+v", name, p)
		}
		if _, err := HostOf(p.URL, false); err != nil {
			t.Errorf("preset %q: %v", name, err)
		}
	}
}
