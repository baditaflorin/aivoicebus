package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
)

// reKeyInLink is a file link that carries its key: a node's #KEY or the pipe's #ID~KEY.
var reKeyInLink = regexp.MustCompile(`(/v1/files/[A-Za-z0-9_-]{22}#|#[A-Za-z0-9_-]{4,64}~)[0-9a-f]{64}`)

// storedBody is the body of message id as the node stores it (what anyone who reads the board sees).
func storedBody(t *testing.T, id string) string {
	t.Helper()
	recs, err := (&client.Client{URLs: []string{os.Getenv("AIVOICEBUS_URL")}, Token: "tok"}).Sync(map[string]uint64{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.Msg != nil && r.Msg.ID == id {
			return r.Msg.Body
		}
	}
	t.Fatalf("message %s is not on the node", id)
	return ""
}

// machine switches the CLI to another machine: its own HOME (so its own machine keys) and identity.
func machine(t *testing.T, home, me string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("AIVOICEBUS_ME", me)
}

func boxPriv(t *testing.T, home string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".config/aivoicebus/machine.box"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A file to one machine whose key is known goes with its key sealed for that machine: the board
// holds no usable key, another machine (even one claiming the same name) cannot open it, and the
// right one fetches it.
func TestSendFileSealsTheKeyForTheRecipientMachine(t *testing.T) {
	for _, via := range []string{"pipe", "node"} {
		t.Run(via, func(t *testing.T) {
			mesh(t)
			services(t)
			air, imac, impostor := t.TempDir(), t.TempDir(), t.TempDir()

			// air says something signed, so its box key is on the board
			machine(t, air, "air/codex")
			if code, _, e := run(t, "", "send", "--to", "imac/claude", "hello from air via "+via); code != 0 {
				t.Fatalf("air's hello: %d %s", code, e)
			}

			src := filepath.Join(t.TempDir(), "report.txt")
			content := "build 42 passed via " + via + "\n"
			if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			machine(t, imac, "imac/claude")
			code, out, e := run(t, "", "send", "--to", "air/codex", "--file", src, "--via", via, "the report")
			if code != 0 || strings.Contains(e, "the key is in the link") {
				t.Fatalf("send --file: %d\n%s\n%s", code, out, e)
			}
			id := strings.Fields(out)[1]

			// The board: the link without its key, the seal, the note for older readers. Opening the
			// seal with air's key gives the real link, whose key is nowhere in the stored message.
			body := storedBody(t, id)
			if reKeyInLink.MatchString(body) || !strings.Contains(body, " sealed=v1.") || !strings.Contains(body, " for=air ") ||
				!strings.Contains(body, attach.SealedNote+" the key to this file is sealed for air's machine key") {
				t.Fatalf("stored body:\n%s", body)
			}
			l, _, _ := attach.Find(body)
			opened, err := attach.OpenWith(l, boxPriv(t, air))
			if err != nil {
				t.Fatal(err)
			}
			if !reKeyInLink.MatchString(opened.URL) {
				t.Fatalf("the opened link carries no key: %s", opened.URL)
			}
			fileKey := opened.URL[len(opened.URL)-64:]
			if strings.Contains(body, fileKey) {
				t.Fatal("the stored message holds the file's key")
			}

			// The reader's view says who can open it and shows neither the seal nor the note.
			machine(t, air, "air/codex")
			_, inbox, _ := run(t, "", "inbox")
			if !strings.Contains(inbox, "its key is sealed for air: only that machine opens it") || strings.Contains(inbox, "sealed=") ||
				strings.Contains(inbox, attach.SealedNote) {
				t.Fatalf("inbox:\n%s", inbox)
			}

			// Another machine with another key (here even under air's own name) cannot open it, and
			// trying does not burn the file.
			machine(t, impostor, "air/codex")
			if code, out, e := run(t, "", "get", id); code == 0 || !strings.Contains(e, "sealed for another machine") {
				t.Fatalf("another machine's get: %d\n%s\n%s", code, out, e)
			}

			machine(t, air, "air/codex")
			code, out, e = run(t, "", "get", id, "--out", filepath.Join(t.TempDir(), "got.txt"))
			if code != 0 {
				t.Fatalf("air's get: %d\n%s\n%s", code, out, e)
			}
			got, _ := os.ReadFile(strings.Fields(out)[1])
			if string(got) != content {
				t.Fatalf("air got %q", got)
			}
		})
	}
}

// Where no single machine's key is known, the link carries its key as before, and the sender is
// told so: a broadcast, a machine that never signed anything, and --open-link.
func TestSendFileFallsBackToTheKeyInTheLinkAndSaysSo(t *testing.T) {
	mesh(t)
	services(t)
	air, imac := t.TempDir(), t.TempDir()
	machine(t, air, "air/codex")
	if code, _, e := run(t, "", "send", "--to", "imac/claude", "hello, air is here"); code != 0 {
		t.Fatalf("air's hello: %d %s", code, e)
	}
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("plain notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	machine(t, imac, "imac/claude")
	present(t, "far/claude")
	machine(t, imac, "imac/claude")
	for _, c := range []struct {
		name string
		args []string
		why  string
	}{
		{"broadcast", []string{"--to", "all", "--keep"}, "it goes to all, not one machine"},
		{"unknown key", []string{"--to", "far/claude"}, "no key is known for far"},
		{"asked", []string{"--to", "air/codex", "--open-link"}, "--open-link"},
	} {
		t.Run(c.name, func(t *testing.T) {
			args := append(append([]string{"send"}, c.args...), "--file", src, "notes for "+c.name, "--force")
			code, out, e := run(t, "", args...)
			if code != 0 || !strings.Contains(e, "the key is in the link ("+c.why) || !strings.Contains(e, "whoever can read the board can fetch the file once") {
				t.Fatalf("%d\n%s\n%s", code, out, e)
			}
			body := storedBody(t, strings.Fields(out)[1])
			if !reKeyInLink.MatchString(body) || strings.Contains(body, "sealed=") || strings.Contains(body, attach.SealedNote) {
				t.Fatalf("stored body:\n%s", body)
			}
		})
	}
	// a dry run says which it would be, before anything is uploaded
	code, out, _ := run(t, "", "send", "--to", "air/codex", "--file", src, "--dry-run", "a look first")
	if code != 0 || !strings.Contains(out, "sealed for air's machine key") {
		t.Fatalf("dry run: %d\n%s", code, out)
	}
}

// The key is sealed for the box key pinned here for air (the first one seen, as ssh pins a host
// key), never for whatever key a later message under air's name carries.
func TestSendFileSealsForThePinnedKeyNotANewOne(t *testing.T) {
	mesh(t)
	services(t)
	air, air2, imac := t.TempDir(), t.TempDir(), t.TempDir()
	machine(t, air, "air/codex")
	run(t, "", "send", "--to", "imac/claude", "air, the first")
	machine(t, imac, "imac/claude")
	if code, _, e := run(t, "", "inbox"); code != 0 { // pins air's first key
		t.Fatal(e)
	}
	machine(t, air2, "air/codex") // air reinstalled, or someone else using the name
	run(t, "", "send", "--to", "imac/claude", "air, the second")
	src := filepath.Join(t.TempDir(), "x.txt")
	if err := os.WriteFile(src, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	machine(t, imac, "imac/claude")
	code, out, e := run(t, "", "send", "--to", "air/codex", "--file", src, "for air")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	l, _, _ := attach.Find(storedBody(t, strings.Fields(out)[1]))
	if l.Sealed == "" {
		t.Fatal("not sealed")
	}
	if _, err := attach.OpenWith(l, boxPriv(t, air2)); err == nil {
		t.Fatal("sealed for the new key")
	}
	if _, err := attach.OpenWith(l, boxPriv(t, air)); err != nil {
		t.Fatalf("not sealed for the pinned key: %v", err)
	}
}
