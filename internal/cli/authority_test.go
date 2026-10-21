package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/secrets"
)

// secrets.Authority knows the two shapes by their text (it cannot import attach or msg); this keeps
// that text equal to what they write: a file's [attach] lines are never a finding, and the
// approval body `aivoicebus approve` sends always is one (outside an approval reply).
func TestAuthorityAgreesWithTheCarriersAndTheApprovalBody(t *testing.T) {
	h := sha256.Sum256([]byte("a key"))
	link := "https://pipe.example.test/#rx3wiitC~" + hex.EncodeToString(h[:])
	for _, l := range []attach.Link{
		{URL: link},
		{URL: link, Name: "report.tgz", Size: 100, SHA256: hex.EncodeToString(h[:])},
		{URL: "https://pipe.example.test/#rx3wiitC", Name: "report.tgz", Sealed: "v1." + strings.Repeat("A", 80), For: "lap"},
	} {
		if f, found := secrets.Authority("the text\n"+l.Lines(), false); found {
			t.Errorf("the carrier %q is read as a %s", l.Line(), f.Kind)
		}
	}
	// the link written by hand is, which is the point of the guard
	if f, found := secrets.Authority("the text\n"+strings.TrimPrefix(attach.Link{URL: link}.Line(), "[attach] "), false); !found || f.Kind != secrets.KindPipeLink {
		t.Errorf("a link without its [attach] marker: %+v %v", f, found)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	body := msg.ApprovalBody("approve", ed25519.Sign(priv, msg.ApprovalStatement("0123456789ab", "approve", "lap/codex.worker")))
	if f, found := secrets.Authority(body, false); !found || f.Kind != secrets.KindApproverSignature {
		t.Errorf("an approval body outside an approval reply: %+v %v", f, found)
	}
	if _, found := secrets.Authority(body, true); found {
		t.Error("an approval body in an approval reply is refused")
	}
}
