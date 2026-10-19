package attach

import (
	"crypto/ecdh"
	"crypto/rand"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func boxPair(t *testing.T) (priv, pub []byte) {
	t.Helper()
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k.Bytes(), k.PublicKey().Bytes()
}

func TestKeyless(t *testing.T) {
	key := strings.Repeat("ab", 32)
	for _, c := range []struct{ in, want string }{
		{"https://node.test/v1/files/AAAAAAAAAAAAAAAAAAAAAA#" + key, "https://node.test/v1/files/AAAAAAAAAAAAAAAAAAAAAA"},
		{"https://pipe.test/#Abc123~" + key, "https://pipe.test/#Abc123"},
		{"https://pipe.test/#Abc123", ""},
		{"https://tail.test/stream-name-long", ""},
		{"https://node.test/v1/files/x#" + key[:63], ""},
	} {
		got, ok := Keyless(c.in)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("Keyless(%q) = %q %v, want %q", c.in, got, ok, c.want)
		}
	}
}

// Only the machine a key was sealed for opens it; another machine's key, an altered seal and a
// seal moved onto another file's link are refused.
func TestSealedKeyOpensOnlyForItsMachine(t *testing.T) {
	airPriv, airPub := boxPair(t)
	otherPriv, _ := boxPair(t)
	key := strings.Repeat("c3", 32)
	for _, link := range []string{
		"https://node.test/v1/files/AAAAAAAAAAAAAAAAAAAAAA#" + key,
		"https://pipe.test/#Id12345~" + key,
	} {
		l := Link{URL: link, Name: "report.txt", Size: 10, SHA256: strings.Repeat("0", 64)}
		sl, err := SealFor(l, airPub, "air")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sl.Line(), key) || strings.Contains(sl.URL, "~") || sl.For != "air" {
			t.Fatalf("the sealed line holds the key: %s", sl.Line())
		}
		got, err := OpenWith(sl, airPriv)
		if err != nil || got.URL != link || got.Sealed != "" || got.Name != "report.txt" {
			t.Fatalf("air could not open it: %+v %v", got, err)
		}
		if _, err := OpenWith(sl, otherPriv); !errors.Is(err, ErrSealedElsewhere) {
			t.Errorf("another machine's key: %v, want ErrSealedElsewhere", err)
		}
		// every byte of the seal is covered: flip one character anywhere in the base64
		for _, at := range []int{3, len(sl.Sealed) / 2, len(sl.Sealed) - 1} {
			bad := sl
			b := []byte(bad.Sealed)
			if b[at] == 'A' {
				b[at] = 'B'
			} else {
				b[at] = 'A'
			}
			bad.Sealed = string(b)
			if _, err := OpenWith(bad, airPriv); err == nil {
				t.Errorf("a seal altered at %d opened", at)
			}
		}
		moved := sl
		moved.URL = strings.Replace(sl.URL, "AAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBB", 1)
		moved.URL = strings.Replace(moved.URL, "Id12345", "Id99999", 1)
		if _, err := OpenWith(moved, airPriv); !errors.Is(err, ErrSealedElsewhere) {
			t.Errorf("a seal moved to another file opened: %v", err)
		}
	}
	if _, err := SealFor(Link{URL: "https://pipe.test/#Id12345"}, airPub, "air"); err == nil {
		t.Error("sealed a link with no key")
	}
	if _, err := SealFor(Link{URL: "https://pipe.test/#Id12345~" + key}, airPub[:31], "air"); err == nil {
		t.Error("sealed for something that is not an X25519 key")
	}
}

// The line round-trips through Find, the view shows a summary that names the machine and drops
// the note for older readers, and a reader that does not know sealed= (0.10.4 and older) is told
// what it is looking at instead of being handed garbage.
func TestSealedLineFindRenderAndAnOldReader(t *testing.T) {
	t.Setenv("AIVOICEBUS_PIPE_URL", "https://pipe.test")
	t.Setenv("AIVOICEBUS_URL", "https://node.test")
	_, airPub := boxPair(t)
	key := strings.Repeat("d4", 32)
	sl, err := SealFor(Link{URL: "https://node.test/v1/files/AAAAAAAAAAAAAAAAAAAAAA#" + key, Name: "a b.txt", Size: 2048}, airPub, "air")
	if err != nil {
		t.Fatal(err)
	}
	body := "the report\n" + sl.Lines()
	if !strings.Contains(body, SealedNote+" the key to this file is sealed for air's machine key: only aivoicebus newer than 0.10.4 on air opens it") {
		t.Fatalf("no note for older readers:\n%s", body)
	}
	got, found, err := Find(body)
	if !found || err != nil || got != sl {
		t.Fatalf("Find: %+v %v %v, want %+v", got, found, err, sl)
	}
	if ProviderFor(got) == nil {
		t.Fatal("a sealed node link has no provider")
	}
	r := Render("m1", body)
	if !strings.Contains(r, "held by a node") || !strings.Contains(r, "its key is sealed for air: only that machine opens it") ||
		strings.Contains(r, SealedNote) || strings.Contains(r, "sealed=") || strings.Contains(r, "[attach]") {
		t.Fatalf("render:\n%s", r)
	}

	// An old reader parses the line as a URL and fields it does not know: the keyless link has no
	// provider there, so it shows "not set up to use" and the note says why.
	old := regexp.MustCompile(` (for|sealed)=\S+`).ReplaceAllString(body, "")
	if ol, _, _ := Find(old); ProviderFor(ol) != nil {
		t.Fatal("an old reader would try to fetch a keyless link")
	}
	if r := Render("m1", old); !strings.Contains(r, "not set up to use") || !strings.Contains(r, SealedNote+" the key to this file is sealed for air") {
		t.Fatalf("an old reader's view:\n%s", r)
	}

	if _, _, err := Find("[attach] https://node.test/v1/files/AAAAAAAAAAAAAAAAAAAAAA sealed=v2.xyz"); err == nil {
		t.Error("a seal this release cannot read was taken")
	}
}
