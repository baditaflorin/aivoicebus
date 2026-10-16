package outbox

import (
	"os"
	"strings"
	"testing"
)

// fragments are overlapping pieces of s, each long enough that finding one in an output means the
// text itself was printed.
func fragments(s string, n int) []string {
	var out []string
	for i := 0; i+n <= len(s); i += 3 {
		out = append(out, s[i:i+n])
	}
	return out
}

// separators are what a hostile sender puts inside a token to keep a detector from matching the raw
// text while it still reads as the token on a screen: zero-width, format and control characters.
var separators = map[string]string{
	"zero-width space":     "\u200b",
	"word joiner":          "\u2060",
	"byte order mark":      "\ufeff",
	"soft hyphen":          "\u00ad",
	"right-to-left mark":   "\u200f",
	"a control character":  "\x01",
	"an escape character":  "\x1b",
	"a zero-width joiner":  "\u200d",
	"a left-to-right mark": "\u200e",
}

func split(s, sep string, n int) string {
	var b strings.Builder
	for i := 0; i < len(s); i += n {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(s[i:min(i+n, len(s))])
	}
	return b.String()
}

func TestCredentialKindSeesTheTextAsItWouldBePrinted(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("aB3dE", 8)
	if k, ok := credentialKind("use " + token); !ok || k != "GitHub token" {
		t.Fatalf("the raw text: %q %v", k, ok)
	}
	for name, sep := range separators {
		text := "use " + split(token, sep, 6) + " now"
		if _, ok := credentialShape(text); ok {
			t.Fatalf("%s: the raw text must not match for this test to mean anything", name)
		}
		if k, ok := credentialKind(text); !ok || k != "GitHub token" {
			t.Errorf("%s: %q %v", name, k, ok)
		}
	}
	if _, ok := credentialKind("deploy window moved to 5pm\u200b, thanks"); ok {
		t.Error("an invisible character is no credential")
	}
}

func TestNoOutputPrintsACredentialSplitByInvisibleCharacters(t *testing.T) {
	token := "gh" + "p_" + strings.Repeat("aB3dE", 8)
	for name, sep := range separators {
		for _, dry := range []bool{true, false} {
			c, s := retractNode(t, meID, nil)
			m := put(t, s, meID, reader1, "see "+split(token, sep, 6)+" thanks")
			out := mustRetract(t, c, m.ID, false, dry, "")
			has(t, out, "message "+m.ID+" to "+reader1+": "+withheld, "the text looks like a GitHub token", "(--leak)")
			for _, f := range fragments(token, 8) {
				lacks(t, out, f)
			}
			lacks(t, out, "see ", "thanks")
			if !dry {
				if r := s.Thread(m.ID)[0].Retracted; r == nil || !r.Leak {
					t.Errorf("%s: recorded as a leak: %+v", name, r)
				}
			}
		}
	}
}

func TestASecretThisMachineHoldsIsNotPrintedWhenSplitByInvisibleCharacters(t *testing.T) {
	c, s := retractNode(t, meID, nil)
	const tok = "wUrKjC9xQrlSj7o0uzjugOIwbCxn6GMNLkApMfF9"
	dir := os.Getenv("HOME") + "/.config/aivoicebus"
	os.MkdirAll(dir, 0o700)
	os.WriteFile(dir+"/observer-token", []byte(tok+"\n"), 0o600)
	for name, sep := range separators {
		m := put(t, s, meID, reader1, "use "+split(tok, sep, 5)+" for the dashboard "+name)
		out := mustRetract(t, c, m.ID, false, true, "")
		has(t, out, withheld, "secret from this machine's aivoicebus config")
		for _, f := range fragments(tok, 8) {
			lacks(t, out, f)
		}
		lacks(t, out, "for the dashboard")
	}
}

func TestExplicitLeakNeverPrintsTheTextWhateverItLooksLike(t *testing.T) {
	for _, dry := range []bool{true, false} {
		c, s := retractNode(t, meID, nil)
		m := put(t, s, meID, reader1, "ordinary words that the sender says were secret")
		out := mustRetract(t, c, m.ID, true, dry, "")
		has(t, out, "message "+m.ID+" to "+reader1+": "+withheldFlagged)
		lacks(t, out, "ordinary words", "were secret")
	}
	// without --leak the same words are shown, so it is the flag that withholds them
	c, s := retractNode(t, meID, nil)
	m := put(t, s, meID, reader1, "ordinary words")
	has(t, mustRetract(t, c, m.ID, false, true, ""), `"ordinary words"`)
}
