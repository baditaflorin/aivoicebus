package secrets

import (
	"strings"
	"testing"
)

func TestACanaryIsRefusedWhereverItAppearsAndNeverEchoed(t *testing.T) {
	tok := token(48)
	c := NewCanaries(KindMeshToken)
	c.Add(tok)
	for _, body := range []string{tok, "the token is " + tok + " ok", "x" + tok + "y", "token=" + tok + "\nmore"} {
		f, ok := c.Find(body)
		if !ok || !f.Strong || f.Kind != KindMeshToken {
			t.Errorf("%q: %+v %v", body, f, ok)
			continue
		}
		if r := Refusal(f); strings.Contains(r, tok) || !strings.Contains(r, KindMeshToken) {
			t.Errorf("the refusal names the kind and never the value: %q", r)
		}
	}
	for _, body := range []string{"", "no secret here", tok[:47], strings.ToUpper(tok), "a different " + token(47) + "Z"} {
		if f, ok := c.Find(body); ok {
			t.Errorf("%q must not match: %+v", body, f)
		}
	}
}

func TestACanarySetIgnoresValuesThatAreTooShortAndTrimsTheRest(t *testing.T) {
	short := strings.Repeat("a", MinCanaryLen-1)
	exact := strings.Repeat("b", MinCanaryLen)
	c := NewCanaries(KindLocalSecret)
	c.Add(short)
	c.Add("")
	c.Add("   \n")
	if _, ok := c.Find("text " + short + " text"); ok {
		t.Error("a value shorter than MinCanaryLen could be ordinary text and must be ignored")
	}
	c.Add("  " + exact + "\n")
	if _, ok := c.Find("hello " + exact); !ok {
		t.Error("a value of exactly MinCanaryLen counts, once trimmed")
	}
	if _, ok := (*Canaries)(nil).Find(exact); ok {
		t.Error("no set, no match")
	}
	if _, ok := NewCanaries(KindMeshToken).Find(exact); ok {
		t.Error("an empty set matches nothing")
	}
}

// What the pattern rules cannot do and the canaries can. A signing key whose base64 has a +
// or / is cut into short runs, so no shape recognises it; the file it was read from does.
func TestASigningKeyIsRefusedByItsFileEvenWhenNoShapeRecognisesIt(t *testing.T) {
	key := signingKey("aivoicebus corpus signing seed 2") // its base64 has + and /
	if !strings.ContainsAny(key, "+/") {
		t.Fatal("this case needs a key with a + or /")
	}
	body := "the release key is " + key + " keep it safe"
	if f, ok := Strong(body); ok {
		t.Fatalf("by shape alone this is not a credential, got %+v", f)
	}
	c := NewCanaries(KindLocalSecret)
	c.Add(key + "\n") // the file as release.SavePrivateKey writes it
	if f, ok := c.Find(body); !ok || f.Kind != KindLocalSecret {
		t.Errorf("%+v %v", f, ok)
	}
}
