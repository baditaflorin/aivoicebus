package cli

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

const ownerSubject = "0mcp:vm701"

// ownerMesh is a node with two sessions on imac: pve1, which claims the role pve (the hypervisor
// workstream), and b5c9, which does not, and a record by the hypervisor session saying imac owns the
// subject through that role.
func ownerMesh(t *testing.T) {
	t.Helper()
	mesh(t)
	t.Chdir(t.TempDir()) // not a git checkout: labels come only from the environment
	session(t, "imac/claude.pve1", "role=pve,tool=claude")
	run(t, "", "inbox")
	session(t, "imac/claude.b5c9", "tool=claude")
	run(t, "", "inbox")
	session(t, "imac/claude.fce3", "tool=claude")
	if code, out, e := run(t, "", "owner", "set", ownerSubject, "--role", "pve", "--person", "florin"); code != 0 {
		t.Fatalf("owner set: %d %s %s", code, out, e)
	}
}

// The machine that owns a subject records it once; anyone on the mesh can look it up, and sees who
// declared it, how old it is, that it is signed and that they did not pin it. Another session of the
// declaring machine (a restart) can remove it; then nobody owns it.
func TestOwnerSetGetListAndRemove(t *testing.T) {
	ownerMesh(t)
	session(t, "boss/claude.a", "")
	code, out, e := run(t, "", "owner", "get", ownerSubject)
	if code != 0 {
		t.Fatalf("owner get: %d %s %s", code, out, e)
	}
	for _, want := range []string{"owner:0mcp:vm701 -> imac needs role:pve (florin)", "record by imac/claude.fce3", "signed, not pinned", "a message would go to imac/claude.pve1"} {
		if !strings.Contains(out, want) {
			t.Errorf("owner get lacks %q:\n%s", want, out)
		}
	}
	_, out, _ = run(t, "", "owner", "get", ownerSubject, "--json")
	var got struct {
		Subject, Machine, By, Check, To string
		Needs                           []string
		Pinned, Person                  bool
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Machine != "imac" || got.Check != "signed" || got.Pinned || got.To != "imac/claude.pve1" ||
		len(got.Needs) != 1 || got.Needs[0] != "role:pve" || got.By != "imac/claude.fce3" || got.Person {
		t.Errorf("owner get --json: %v\n%s", err, out)
	}
	if _, out, _ = run(t, "", "owner", "list"); !strings.Contains(out, "0mcp:vm701") || !strings.Contains(out, "role:pve") || !strings.Contains(out, "florin") || !strings.Contains(out, "signed") {
		t.Errorf("owner list:\n%s", out)
	}
	if _, out, _ = run(t, "", "owner", "list", "service:*"); !strings.Contains(out, "no owner records") {
		t.Errorf("owner list with a glob nothing matches:\n%s", out)
	}
	// the lease commands leave the registry alone
	if _, out, _ = run(t, "", "lease", "list"); !strings.Contains(out, "no leases held") {
		t.Errorf("an owner record is not a lease to check before editing:\n%s", out)
	}
	if _, out, _ = run(t, "", "lease", "list", "owner:*"); !strings.Contains(out, "owner:0mcp:vm701") {
		t.Errorf("lease list owner:* shows them:\n%s", out)
	}
	for _, args := range [][]string{{"lease", "take", "owner:0mcp:vm701"}, {"lease", "release", "owner:0mcp:vm701"}, {"lease", "run", "owner:x", "--", "true"}} {
		if code, _, e := run(t, "", args...); code != exitUsage || !strings.Contains(e, "aivoicebus owner set SUBJECT") {
			t.Errorf("%v: %d %q", args, code, e)
		}
	}

	// a restarted session of the declaring machine removes it; a session of another machine cannot
	if code, _, e := run(t, "", "owner", "rm", ownerSubject); code != exitNotFound || !strings.Contains(e, "declared no owner record") {
		t.Errorf("boss removing another machine's record: %d %q", code, e)
	}
	session(t, "imac/claude.restarted", "")
	if code, out, e := run(t, "", "owner", "rm", ownerSubject); code != 0 || !strings.Contains(out, "removed 1 record") {
		t.Fatalf("owner rm: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	if code, _, e := run(t, "", "owner", "get", ownerSubject); code != exitNotFound || !strings.Contains(e, "no owner is recorded for 0mcp:vm701") {
		t.Errorf("after rm: %d %q", code, e)
	}
}

// A new record from the same machine replaces its old one (no conflict with itself), and renew
// extends what this machine declared and nothing else.
func TestOwnerSetReplacesAndRenewExtends(t *testing.T) {
	ownerMesh(t)
	if code, out, e := run(t, "", "owner", "set", ownerSubject, "--machine", "imac", "--ttl", "10d"); code != 0 || !strings.Contains(out, "replaces 1 earlier record") {
		t.Fatalf("second set: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	_, out, _ := run(t, "", "owner", "get", ownerSubject)
	if strings.Contains(out, "needs") || !regexp.MustCompile(`expires in 2(39|40)h`).MatchString(out) {
		t.Errorf("the newer record (no needs, 10 days) stands alone:\n%s", out)
	}
	if code, _, e := run(t, "", "owner", "renew"); code != exitNotFound || !strings.Contains(e, "declared no owner record to renew") {
		t.Errorf("boss has nothing to renew: %d %q", code, e)
	}
	session(t, "imac/claude.fce3", "")
	if code, out, e := run(t, "", "owner", "renew"); code != 0 || !strings.Contains(out, "owner:0mcp:vm701 renewed until") {
		t.Fatalf("renew: %d %q %q", code, out, e)
	}
	session(t, "boss/claude.a", "")
	if _, out, _ = run(t, "", "owner", "get", ownerSubject); !regexp.MustCompile(`expires in 7(19|20)h`).MatchString(out) {
		t.Errorf("renewed for 30 days:\n%s", out)
	}
	if _, out, _ = run(t, "", "owner", "list", "--json"); strings.Count(out, `"subject"`) != 1 {
		t.Errorf("one record, not a pile of renewals:\n%s", out)
	}
}

// A family record covers every subject under it, and an exact one beats it.
func TestOwnerFamilyRecordsAndMistypedSubjects(t *testing.T) {
	mesh(t)
	t.Chdir(t.TempDir())
	present(t, "imac/claude.pve1")
	session(t, "imac/claude.fce3", "")
	if code, _, e := run(t, "", "owner", "set", "0mcp:*"); code != 0 {
		t.Fatalf("a family record: %d %q", code, e)
	}
	for _, args := range [][]string{{"owner", "set", "*"}, {"owner", "set", "vm*"}, {"owner", "set", "VM 701"}, {"owner", "set", "0mcp:vm701", "--person", "two words"}, {"owner", "set", "0mcp:vm701", "--machine", "a/b"}, {"owner", "set"}} {
		if code, _, _ := run(t, "", args...); code != exitUsage {
			t.Errorf("%v: %d, want a usage error", args, code)
		}
	}
	session(t, "boss/claude.a", "")
	if code, out, _ := run(t, "", "owner", "get", "0mcp:vm999"); code != 0 || !strings.Contains(out, "via 0mcp:*") {
		t.Errorf("the family covers vm999: %d %q", code, out)
	}
	if code, _, e := run(t, "", "owner", "get", "1docker:vm1"); code != exitNotFound || !strings.Contains(e, "aivoicebus owner set 1docker:vm1") {
		t.Errorf("a subject nobody covers says what to do: %d %q", code, e)
	}
}
