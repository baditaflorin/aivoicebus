package secrets

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
)

// pipeLinkOf is a link of the shape the pipe issues, built at run time (see corpus_test.go: no
// credential-shaped literal is committed). hexn is the corpus's own helper.
func pipeLinkOf(host, id string) string {
	return "https://" + host + "/#" + id + "~" + hexn("key "+id, 64)
}

// sigLine is an approver signature line in the shape msg.ApprovalBody writes, with a real signature
// over nothing in particular: only its shape matters here (a test in package cli ties the two).
func sigLine() string {
	_, priv, _ := ed25519.GenerateKey(strings.NewReader(strings.Repeat("seed", 64)))
	return "[approver-sig v1 " + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, []byte("x"))) + "]"
}

func TestAuthorityFindsAPipeLinkWithItsKeyAndAnApproverSignature(t *testing.T) {
	link := pipeLinkOf("pipe.example.test", "rx3wiitC")
	for _, c := range []struct {
		name, body string
		decision   bool
		kind       string
	}{
		{"a link pasted in a sentence", "LINK (5 minutes, no backslashes): " + link + "\nRun steps 1-3 now.", false, KindPipeLink},
		{"a link alone", link, false, KindPipeLink},
		{"another host", pipeLinkOf("p.fleet.example", "gVpI7wOt"), false, KindPipeLink},
		{"http", strings.Replace(link, "https", "http", 1), false, KindPipeLink},
		{"a shell paste puts backslashes before # and ~", strings.NewReplacer("#", `\#`, "~", `\~`).Replace(link), false, KindPipeLink},
		{"in a markdown link", "[the file](" + link + ")", false, KindPipeLink},
		{"the decision flag does not excuse a link", "approve\n" + link, true, KindPipeLink},
		{"a signature line after the word", "approve\n" + sigLine(), false, KindApproverSignature},
		{"a signature in prose", "here it is " + sigLine() + " again", false, KindApproverSignature},
		{"a newer signature version", strings.Replace(sigLine(), "v1", "v2", 1), false, KindApproverSignature},
	} {
		f, ok := Authority(c.body, c.decision)
		if !ok || f.Kind != c.kind || !f.Strong {
			t.Errorf("%s: %+v %v, want %s", c.name, f, ok, c.kind)
		}
	}
}

func TestAuthorityLeavesOrdinaryTextAndTheSanctionedCarrierAlone(t *testing.T) {
	link := pipeLinkOf("pipe.example.test", "rx3wiitC")
	for _, c := range []struct {
		name, body string
		decision   bool
	}{
		{"the [attach] line send --file writes", "the report\n[attach] " + link + " name=report.tgz size=100", false},
		{"the same line with leading space", "text\n   [attach] " + link, false},
		{"a sealed link: the key is not in it", "[attach] https://pipe.example.test/#rx3wiitC name=a sealed=v1.abc", false},
		{"a link without a key", "see https://pipe.example.test/#rx3wiitC for it", false},
		{"the shape with placeholders", "links look like https://HOST/#ID~KEY", false},
		{"a key one character short", "https://pipe.example.test/#rx3wiitC~" + hexn("k", 63), false},
		{"a key of non-hex", "https://pipe.example.test/#rx3wiitC~" + strings.Repeat("z", 64), false},
		{"a sha256 that is not on a link", "sha256: " + hexn("file", 64), false},
		{"a link with a path", "https://example.test/a/b/#frag~" + hexn("k", 64)[:10], false},
		{"the word approve and nothing signed", "approve", false},
		{"a signature in an approval reply (the person's decision)", "approve\n" + sigLine(), true},
		{"a masked signature", "approve\n[approver-sig v1 [redacted:token]]", false},
		{"talk about the tag", "an approver-sig line is what `aivoicebus approve` sends", false},
		{"empty", "", false},
	} {
		if f, ok := Authority(c.body, c.decision); ok {
			t.Errorf("%s: found a %s in %q", c.name, f.Kind, c.body)
		}
	}
}

// The refusal says what to do instead, and what it points to exists.
func TestAuthorityRefusalsPointToTheRightCommand(t *testing.T) {
	pl := Refusal(Finding{Kind: KindPipeLink, Strong: true})
	for _, want := range []string{"pipe link with its key", "aivoicebus send --file", "aivoicebus token hand", "send it again", "looks like it contains a"} {
		if !strings.Contains(pl, want) {
			t.Errorf("pipe link refusal lacks %q: %s", want, pl)
		}
	}
	sg := Refusal(Finding{Kind: KindApproverSignature, Strong: true})
	for _, want := range []string{"approver signature", "aivoicebus approve ID", "typed by the person at a terminal"} {
		if !strings.Contains(sg, want) {
			t.Errorf("signature refusal lacks %q: %s", want, sg)
		}
	}
	if strings.Contains(sg, "send --file") {
		t.Errorf("a signature is not sent as a file: %s", sg)
	}
	// the other kinds keep their words
	if got := Refusal(Finding{Kind: "GitHub token"}); !strings.Contains(got, "say where the secret lives instead") {
		t.Errorf("%s", got)
	}
}

// Find does not change: a pipe link or a signature is Authority's business, so the files a person
// sends and the lines of a log stream are judged as before.
func TestFindDoesNotSeeAuthorityShapes(t *testing.T) {
	for _, body := range []string{pipeLinkOf("pipe.example.test", "rx3wiitC"), "approve\n" + sigLine()} {
		for _, f := range Find(body) {
			if f.Strong {
				t.Errorf("Find reads %q as a strong %s", body, f.Kind)
			}
		}
	}
}
