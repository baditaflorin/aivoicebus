package attach

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestSealOpenRoundTripAndTamper(t *testing.T) {
	key, _ := newKey()
	env, err := seal(key, []byte("hello fleet"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := open(key, env); err != nil || string(got) != "hello fleet" {
		t.Fatalf("open: %q %v", got, err)
	}
	other, _ := newKey()
	if _, err := open(other, env); err != ErrAuth {
		t.Errorf("wrong key: %v, want ErrAuth", err)
	}
	bad := append([]byte(nil), env...)
	bad[len(bad)-6] ^= 1 // flips a bit of the ciphertext (still valid base64 alphabet)
	if _, err := open(key, bad); err == nil {
		t.Error("tampered envelope opened")
	}
	if _, err := open(key, []byte("not base64 !!")); err == nil {
		t.Error("garbage opened")
	}
}

func TestFindAndRender(t *testing.T) {
	t.Setenv("AIVOICEBUS_PIPE_URL", "https://pipe.test")
	t.Setenv("AIVOICEBUS_TAIL_URL", "https://tail.test")
	key := strings.Repeat("ab", 32)
	body := "here you go\n[attach] https://pipe.test/#Xq7k2aPb~" + key + " name=a%20b.txt size=12 sha256=" + strings.Repeat("0", 64)
	l, found, err := Find(body)
	if !found || err != nil || l.Name != "a b.txt" || l.Size != 12 {
		t.Fatalf("Find: %+v %v %v", l, found, err)
	}
	out := Render("m1", body)
	if strings.Contains(out, key) || strings.Contains(out, "Xq7k2aPb") {
		t.Errorf("rendered body leaks the link: %q", out)
	}
	if !strings.Contains(out, "here you go") || !strings.Contains(out, "aivoicebus get m1") {
		t.Errorf("rendered body: %q", out)
	}
	if Render("m1", out) != out {
		t.Error("Render is not idempotent")
	}
	if Render("m1", "plain") != "plain" {
		t.Error("plain body changed")
	}

	// A link to a host this machine is not configured for is never fetched, and says so.
	evil := "[attach] https://evil.example/#Xq7k2aPb~" + key
	if ProviderFor(Link{URL: "https://evil.example/#Xq7k2aPb~" + key}) != nil {
		t.Error("an unconfigured host got a provider")
	}
	if r := Render("m2", evil); strings.Contains(r, "evil.example") || !strings.Contains(r, "not set up") {
		t.Errorf("evil render: %q", r)
	}
	// Lookalike hosts and shapes are not handled either.
	for _, u := range []string{
		"https://pipe.test.evil.example/#Xq7k2aPb~" + key,
		"http://pipe.test/#Xq7k2aPb~" + key,
		"https://pipe.test/#Xq7k2aPb~short",
		"https://pipe.test/Xq7k2aPb",
		"https://pipe.test/?x=1#Xq7k2aPb~" + key,
		"https://user@pipe.test/#Xq7k2aPb~" + key,
		"https://tail.test/short",
		"https://tail.test/aaaaaaaaaaaaaaaa?follow=1",
	} {
		if ProviderFor(Link{URL: u}) != nil {
			t.Errorf("handled %s", u)
		}
	}
	if ProviderFor(Link{URL: "https://tail.test/aaaaaaaaaaaaaaaa"}) == nil {
		t.Error("a valid tail link has no provider")
	}
}

func TestDefaultsAndOff(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_PIPE_URL", "")
	t.Setenv("AIVOICEBUS_TAIL_URL", "")
	ps := Providers()
	if len(ps) != 1 || ps[0].(*Pipe).Base != DefaultPipeURL {
		t.Fatalf("with nothing set: %d providers, want only the default pipe", len(ps))
	}
	for _, off := range []string{"off", "OFF", "none"} {
		t.Setenv("AIVOICEBUS_PIPE_URL", off)
		if ps := Providers(); len(ps) != 0 {
			t.Errorf("AIVOICEBUS_PIPE_URL=%s: %d providers", off, len(ps))
		}
	}
	// A node link is recognised only for a node this machine is configured with.
	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:7777,https://node.example")
	ps = Providers()
	n, ok := ps[len(ps)-1].(*Node)
	if !ok {
		t.Fatalf("no node provider with nodes configured")
	}
	key := strings.Repeat("ab", 32)
	id := strings.Repeat("A", 22)
	for raw, want := range map[string]bool{
		"https://node.example/v1/files/" + id + "#" + key:      true,
		"http://127.0.0.1:7777/v1/files/" + id + "#" + key:     true,
		"http://node.example/v1/files/" + id + "#" + key:       false, // scheme differs from the configured one
		"https://evil.example/v1/files/" + id + "#" + key:      false,
		"https://node.example/v1/files/" + id + "?x=1#" + key:  false,
		"https://node.example/v1/files/short#" + key:           false,
		"https://node.example/v1/files/" + id + "#" + key[:63]: false,
		"https://user@node.example/v1/files/" + id + "#" + key: false,
		"https://node.example/v1/release/" + id + "#" + key:    false,
		"https://node.example/v1/files/" + id + "/read#" + key: false,
	} {
		u, _ := url.Parse(raw)
		if got := n.Handles(u); got != want {
			t.Errorf("Handles(%s) = %v, want %v", raw, got, want)
		}
	}
	l := Link{URL: "https://node.example/v1/files/" + id + "#" + key, Name: "r.txt", Size: 5}
	if out := Render("m9", "see\n"+l.Line()); strings.Contains(out, key) || !strings.Contains(out, "held by a node") || !strings.Contains(out, "aivoicebus get m9") {
		t.Errorf("node link rendered as %q", out)
	}
}

func TestBaseURLRefusesPlainHTTP(t *testing.T) {
	t.Setenv("AIVOICEBUS_PIPE_URL", "http://pipe.example")
	if _, err := baseURL("AIVOICEBUS_PIPE_URL", DefaultPipeURL); err == nil {
		t.Error("plain http to a remote host accepted")
	}
	t.Setenv("AIVOICEBUS_PIPE_URL", "http://127.0.0.1:9999/")
	if b, err := baseURL("AIVOICEBUS_PIPE_URL", DefaultPipeURL); err != nil || b != "http://127.0.0.1:9999" {
		t.Errorf("loopback: %q %v", b, err)
	}
	t.Setenv("AIVOICEBUS_PIPE_URL", "https://pipe.example/some/path")
	if _, err := baseURL("AIVOICEBUS_PIPE_URL", DefaultPipeURL); err == nil {
		t.Error("a path in the server URL accepted")
	}
}

func TestSafeNameAndSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"report.tgz":             "report.tgz",
		"../../etc/passwd":       "passwd",
		`..\..\evil.exe`:         "evil.exe",
		".bashrc":                "bashrc",
		"":                       "attachment",
		"...":                    "attachment",
		"a\x1b[31mb\nc":          "a_[31mb_c",
		"we:ird*na?me.txt":       "we_ird_na_me.txt",
		strings.Repeat("x", 300): strings.Repeat("x", 100) + "…",
	} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Sanitize("ok\tline\n\x1b]0;title\x07 ‮"); strings.ContainsAny(got, "\x1b\x07‮") {
		t.Errorf("Sanitize left a control character: %q", got)
	}
}

// fakePipe is just enough of pipe server: POST stores, GET returns (burning when asked).
type fakePipe struct {
	mu    sync.Mutex
	store map[string][]byte
	burn  map[string]bool
	reads int
	query url.Values
}

func newFakePipe(t *testing.T) (*fakePipe, *httptest.Server) {
	f := &fakePipe{store: map[string][]byte{}, burn: map[string]bool{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakePipe) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		id := "Id" + strings.Repeat("a", len(f.store)+6)
		f.store[id], f.burn[id], f.query = b, r.URL.Query().Get("burn") == "1", r.URL.Query()
		w.Header().Set("X-Pipe-Watch", "http://"+r.Host+"/"+id+"?watch=tok")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "http://"+r.Host+"/"+id+"\n")
	case http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/")
		b, ok := f.store[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.reads++
		if f.burn[id] {
			delete(f.store, id)
		}
		w.Write(b)
	}
}

func TestPipePutGetBurnsAndEncrypts(t *testing.T) {
	fp, srv := newFakePipe(t)
	p := &Pipe{Base: srv.URL}
	data := []byte("secret-ish file contents")
	l, watch, err := p.Put(context.Background(), data, true)
	if err != nil {
		t.Fatal(err)
	}
	if watch == "" || l.Size != int64(len(data)) || len(l.SHA256) != 64 {
		t.Errorf("link %+v watch %q", l, watch)
	}
	if fp.query.Get("burn") != "1" || fp.query.Get("ttl") != "300" || fp.query.Get("notify") != "1" {
		t.Errorf("upload query %v", fp.query)
	}
	for _, b := range fp.store {
		if bytes.Contains(b, data) || bytes.Contains(b, []byte("secret")) {
			t.Error("the pipe holds plaintext")
		}
	}
	u, _ := url.Parse(l.URL)
	if !p.Handles(u) {
		t.Fatalf("pipe does not handle its own link %s", l.URL)
	}
	got, err := p.Get(context.Background(), l)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get: %q %v", got, err)
	}
	if _, err := p.Get(context.Background(), l); err != ErrGone {
		t.Errorf("second read: %v, want ErrGone", err)
	}
}

func TestPipeGetIgnoresAHostInTheLink(t *testing.T) {
	_, srv := newFakePipe(t)
	p := &Pipe{Base: srv.URL}
	if _, err := p.Get(context.Background(), Link{URL: "https://evil.example/#Idaaaaaa~" + strings.Repeat("0", 64)}); err == nil {
		t.Error("fetched from a host that is not the configured pipe")
	}
}

func TestTailPushReadMasksAndSanitizes(t *testing.T) {
	var mu sync.Mutex
	var buf strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			buf.Write(b)
			return
		}
		io.WriteString(w, buf.String()+"evil \x1b[2J line\n")
	}))
	defer srv.Close()
	tl := &Tail{Base: srv.URL}
	stream, l, err := tl.NewStream()
	if err != nil || !ValidStream(stream) || len(stream) < 20 {
		t.Fatalf("NewStream %q %v", stream, err)
	}
	lines, masked, err := tl.Push(context.Background(), stream, strings.NewReader("one\nTOKEN=hunter2\ntwo\n"),
		func(s string) (string, bool) {
			if strings.Contains(s, "TOKEN") {
				return "[withheld]", true
			}
			return s, false
		})
	if err != nil || lines != 3 || masked != 1 {
		t.Fatalf("Push: %d %d %v", lines, masked, err)
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Error("a masked line reached the tail")
	}
	var out strings.Builder
	if err := tl.Read(context.Background(), l, &out, 10, false, 1<<20); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") || !strings.Contains(out.String(), "one\n[withheld]\ntwo\n") {
		t.Errorf("Read: %q", out.String())
	}
	if _, _, err := tl.Push(context.Background(), "short", strings.NewReader("x"), nil); err == nil {
		t.Error("a guessable stream name was accepted")
	}
}

// An envelope made by pipe server's own shell helper (_pipe_enc from https://pipe.example.com/shell),
// so a change to the format here that the pipe's clients cannot read fails this test.
func TestOpensTheEnvelopeThePipeHelperMakes(t *testing.T) {
	const key = "aa0f41235cf8f121c6d484a9dacf21a1e4b788ec64c1e2890ccf24107bf8d6bf"
	const env = "AcSpU14e8RDGUhP4ntWHFG9Xy2Qi0vGdzcYeCdsKb1izrairrN8D2umk0oZyqu7UtVavSPuk3IhMaThgVvlWbtA5"
	got, err := open(key, []byte(env))
	if err != nil || string(got) != "from shell to go\n" {
		t.Fatalf("open: %q %v", got, err)
	}
}

// A fetch whose first connection is dropped before any answer is tried once more: an entry that
// was never served was never burned. An answer, even an error, is final.
func TestGetTriesAgainWhenTheFirstAttemptGotNoAnswer(t *testing.T) {
	key, _ := newKey()
	env, _ := seal(key, []byte("image bytes"))
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n == 1 { // drop the connection without answering, as a stalled TLS handshake ends
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.Write(env)
	}))
	defer srv.Close()
	p := &Pipe{Base: srv.URL}
	got, err := p.Get(context.Background(), Link{URL: srv.URL + "/#Abcd1234~" + key})
	if err != nil || string(got) != "image bytes" || hits != 2 {
		t.Fatalf("got %q %v after %d requests", got, err, hits)
	}
	hits = 0
	gone := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.NotFound(w, r)
	}))
	defer gone.Close()
	if _, err := (&Pipe{Base: gone.URL}).Get(context.Background(), Link{URL: gone.URL + "/#Abcd1234~" + key}); err != ErrGone || hits != 1 {
		t.Errorf("a 404 was retried or misread: %v after %d requests", err, hits)
	}
}
