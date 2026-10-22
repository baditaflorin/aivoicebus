package outbox

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The records come back with the verdict on their signature, which is read from the record's own message:
// the lease list says who declared a record, and only the message says whether the machine's key signed it.
func TestOwnerRecordsCarryTheVerdictOnTheirSignature(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c, _ := retractNode(t, "imac/claude.fce3", nil)
	write := func(key, note string, unsigned bool) {
		t.Helper()
		c.NoSign = unsigned
		p, err := Prepare(c, Request{To: "imac/claude.fce3", Body: msg.LeaseBody(key, note), Kind: msg.KindLease, TTL: "30d"}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Send(c); err != nil {
			t.Fatal(err)
		}
	}
	write("owner:0mcp:vm701", "machine=imac", false)
	write("owner:service:gateway", "machine=imac", true)
	write("owner:service:broken", "no machine here", false)
	write("deploy:prod", "not an owner record", false)
	recs, err := OwnerRecords(c, nil)
	if err != nil || len(recs) != 3 {
		t.Fatalf("OwnerRecords: %v %+v", err, recs)
	}
	got := map[string]string{}
	for _, r := range recs {
		got[r.Subject] = r.Verified + "|" + r.Problem
	}
	if got["0mcp:vm701"] != msg.VerifiedSigned+"|" || got["service:gateway"] != msg.VerifiedUnsigned+"|" || !strings.HasPrefix(got["service:broken"], "|") || !strings.Contains(got["service:broken"], "machine") {
		t.Errorf("verdicts: %v", got)
	}
	only, _ := OwnerRecords(c, func(s string) bool { return s == "0mcp:vm701" })
	if len(only) != 1 || only[0].Subject != "0mcp:vm701" {
		t.Errorf("keep narrows the records (and the reads): %+v", only)
	}
	res, err := ResolveOwner(c, "service:gateway")
	if !errors.Is(err, msg.ErrNoOwner) || len(res.Ignored) != 1 || !strings.Contains(res.Ignored[0].Why, "not signed") {
		t.Errorf("an unsigned record is not used: %v %+v", err, res.Ignored)
	}
	if res, err = ResolveOwner(c, "0mcp:vm701"); err != nil || res.Record.Machine != "imac" {
		t.Errorf("a signed one is: %v %+v", err, res)
	}
	if _, err = ResolveOwner(c, "Bad Subject"); err == nil || !strings.Contains(err.Error(), "aivoicebus owner list") {
		t.Errorf("a subject that cannot exist: %v", err)
	}
}
