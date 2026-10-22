package msg

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ownerNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// rec is a signed record declared by by, naming machine, about subject, 3 days old with 27 left.
func rec(id, subject, machine, by string) OwnerRecord {
	return OwnerRecord{Subject: subject, Machine: machine, By: by, ID: id, Verified: VerifiedSigned,
		At: ownerNow.Add(-72 * time.Hour), Until: ownerNow.Add(27 * 24 * time.Hour)}
}

func TestOwnerSubjectGrammar(t *testing.T) {
	for _, s := range []string{"0mcp:vm701", "service:go-fleet-backup", "alias:0mcp_docker_build", "domain:example.com", "repo:baditaflorin/aivoicebus", "0mcp:*", "service:go-*", strings.Repeat("a", MaxOwnerSubject)} {
		if err := ValidOwnerSubject(s); err != nil {
			t.Errorf("ValidOwnerSubject(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", "*", "v*", "a:*", "VM701", "vm 701", "vm701\n", "vm?701", "vm701;rm", "vm@701", strings.Repeat("a", MaxOwnerSubject+1)} {
		if err := ValidOwnerSubject(s); err == nil {
			t.Errorf("ValidOwnerSubject(%q) = nil, want an error", s)
		}
	}
	// a lookup names one thing: no '*'
	if err := ValidOwnerLookup("0mcp:*"); err == nil {
		t.Error("a lookup with * must be refused")
	}
	if err := ValidOwnerLookup("0mcp:vm701"); err != nil {
		t.Errorf("ValidOwnerLookup: %v", err)
	}
	// the longest subject and its prefix is still one valid lease key, and one more is not
	if !ValidLeaseKey(OwnerKey(strings.Repeat("a", MaxOwnerSubject))) || ValidLeaseKey(OwnerKey(strings.Repeat("a", MaxOwnerSubject+1))) {
		t.Errorf("MaxOwnerSubject %d does not match the lease key limit", MaxOwnerSubject)
	}
}

func TestOwnerSubjectOfTarget(t *testing.T) {
	for in, want := range map[string]string{"owner:vm:701": "vm:701", "OWNER:0mcp:VM701": "0mcp:vm701", " owner:x ": "x"} {
		if got, ok := OwnerSubjectOf(in); !ok || got != want {
			t.Errorf("OwnerSubjectOf(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"owner", "owne:x", "imac/claude", "@role", "all", "xowner:y", ""} {
		if _, ok := OwnerSubjectOf(in); ok {
			t.Errorf("OwnerSubjectOf(%q) said yes", in)
		}
	}
}

func TestOwnerNote(t *testing.T) {
	f := OwnerFields{Machine: "imac-m1-imac", Needs: []string{"role:pve", "repo:baditaflorin/x"}, Person: "florin"}
	note := f.Note()
	if note != "machine=imac-m1-imac needs=role:pve,repo:baditaflorin/x person=florin" {
		t.Fatalf("Note() = %q", note)
	}
	got, err := ParseOwnerNote(note)
	if err != nil || !reflect.DeepEqual(got, f) {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	// words without = and keys it does not know are ignored, in any order: a later release may add fields
	got, err = ParseOwnerNote("person=ana free text  tier=gold machine=lap")
	if err != nil || got.Machine != "lap" || got.Person != "ana" || got.Needs != nil {
		t.Errorf("lenient parse: %+v, %v", got, err)
	}
	for name, bad := range map[string]string{
		"no machine":        "person=ana",
		"machine twice":     "machine=a machine=b",
		"person twice":      "machine=a person=x person=y",
		"needs twice":       "machine=a needs=gpu needs=cuda",
		"a path":            "machine=a/b",
		"a selector":        "machine=@repo=x",
		"upper case":        "machine=Imac",
		"all":               "machine=all",
		"the system host":   "machine=aivoicebus",
		"the person's door": "machine=human",
		"bad person":        "machine=a person=Dr.%20X",
		"long person":       "machine=a person=" + strings.Repeat("x", MaxOwnerPerson+1),
		"bad needs":         "machine=a needs=planet:mars",
		"too many needs":    "machine=a needs=a,b,c,d,e,f,g,h,i",
		"long note":         "machine=a " + strings.Repeat("x ", MaxOwnerNote),
		"invisible":         "machine=a​b",
	} {
		if _, err := ParseOwnerNote(bad); err == nil {
			t.Errorf("%s: ParseOwnerNote(%q) was accepted", name, bad)
		}
	}
}

// A lease key has no '*', so a family is written with '@' in the key and read back as '*'; a literal '@' in a
// key is a '*' too, and a subject can never hold one, so the two cannot be confused.
func TestOwnerFamilyKeysAreValidLeaseKeys(t *testing.T) {
	key := OwnerKey("0mcp:*")
	if key != "owner:0mcp:@" || !ValidLeaseKey(key) || ValidLeaseKey("owner:0mcp:*") {
		t.Fatalf("OwnerKey = %q", key)
	}
	r, ok := OwnerOf(Lease{Key: key, By: "imac/claude.fce3", ID: "abcdef0123", Note: "machine=imac"})
	if !ok || r.Problem != "" || r.Subject != "0mcp:*" {
		t.Errorf("OwnerOf(%q) = %+v, %v", key, r, ok)
	}
	if k := OwnerKey("0mcp:vm701"); k != "owner:0mcp:vm701" {
		t.Errorf("an exact subject is written as it is: %q", k)
	}
	if r, _ := OwnerOf(Lease{Key: "owner:@", Note: "machine=imac"}); r.Problem == "" {
		t.Error("a bare family would claim every subject")
	}
}

func TestOwnerOfReadsLeases(t *testing.T) {
	l := Lease{Key: "owner:0mcp:vm701", By: "imac/claude.fce3", ID: "abcdef0123", At: ownerNow, Until: ownerNow.Add(time.Hour), Note: "machine=imac needs=role:pve"}
	r, ok := OwnerOf(l)
	if !ok || r.Problem != "" || r.Subject != "0mcp:vm701" || r.Machine != "imac" || !reflect.DeepEqual(r.Needs, []string{"role:pve"}) || r.By != l.By || r.ID != l.ID {
		t.Fatalf("OwnerOf = %+v, %v", r, ok)
	}
	if _, ok := OwnerOf(Lease{Key: "deploy:prod"}); ok {
		t.Error("a lease in another namespace is not an owner record")
	}
	if _, ok := OwnerOf(Lease{Key: "owners:x"}); ok {
		t.Error("owner: is a prefix of the key, not of the word")
	}
	for name, l := range map[string]Lease{
		"bad note":      {Key: "owner:vm:1", Note: "no machine here"},
		"upper subject": {Key: "owner:VM:1", Note: "machine=a"},
		"star only":     {Key: "owner:*", Note: "machine=a"},
	} {
		r, ok := OwnerOf(l)
		if !ok || r.Problem == "" {
			t.Errorf("%s: want a record with a problem, got %+v %v", name, r, ok)
		}
	}
}

func TestOwnerPins(t *testing.T) {
	got := ParseOwnerPins("# who may say who owns what\nimac-*/claude.*  # comment\nIMAC-*/claude.*\n*\n*/claude.*\nnohost\nlap/codex.a1b2\n\n")
	if want := []string{"imac-*/claude.*", "lap/codex.a1b2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseOwnerPins = %v, want %v", got, want)
	}
}

func resolveOwn(t *testing.T, subject string, pins []string, recs ...OwnerRecord) (OwnerResolution, error) {
	t.Helper()
	return ResolveOwner(subject, recs, pins, ownerNow)
}

func TestOwnerExactBeatsFamilyAndLongerFamilyBeatsShorter(t *testing.T) {
	general := rec("g000000001", "0mcp:*", "hyper", "hyper/claude.aaaa")
	family := rec("f000000001", "0mcp:vm7*", "dbhost", "dbhost/claude.bbbb")
	exact := rec("e000000001", "0mcp:vm701", "imac", "imac/claude.cccc")
	general.At = ownerNow.Add(-time.Hour) // newer than the others: specificity, not age, decides
	for _, tc := range []struct {
		name string
		recs []OwnerRecord
		want string
	}{
		{"exact over families", []OwnerRecord{general, family, exact}, "imac"},
		{"longer family over the general one", []OwnerRecord{general, family}, "dbhost"},
		{"general when nothing else", []OwnerRecord{general}, "hyper"},
	} {
		res, err := resolveOwn(t, "0mcp:vm701", nil, tc.recs...)
		if err != nil || res.Record.Machine != tc.want {
			t.Errorf("%s: %+v, %v; want %s", tc.name, res.Record, err, tc.want)
		}
	}
	if res, err := resolveOwn(t, "0mcp:vm801", nil, general, family, exact); err != nil || res.Record.Machine != "hyper" {
		t.Errorf("a subject only the general record covers: %+v, %v", res.Record, err)
	}
	if _, err := resolveOwn(t, "1docker:vm701", nil, general, family, exact); !errors.Is(err, ErrNoOwner) {
		t.Errorf("a subject no record covers: %v, want ErrNoOwner", err)
	}
}

func TestOwnerOnlyAVerifiedSignatureCounts(t *testing.T) {
	for _, v := range []string{VerifiedUnsigned, VerifiedBad, VerifiedChanged, ""} {
		r := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
		r.Verified = v
		res, err := resolveOwn(t, "0mcp:vm701", nil, r)
		if !errors.Is(err, ErrNoOwner) || len(res.Ignored) != 1 {
			t.Errorf("verdict %q: %v, ignored %d; want ErrNoOwner and the record listed", v, err, len(res.Ignored))
		}
		if v == VerifiedChanged && len(res.Ignored) == 1 && !strings.Contains(res.Ignored[0].Why, "KEY CHANGED") {
			t.Errorf("a changed key must say so: %q", res.Ignored[0].Why)
		}
		if !strings.Contains(err.Error(), "aivoicebus owner get") {
			t.Errorf("with a record ignored the error points at `owner get`: %v", err)
		}
	}
}

func TestOwnerExpiredAndMalformedRecords(t *testing.T) {
	gone := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
	gone.Until = ownerNow
	bad := rec("b000000001", "0mcp:vm701", "", "imac/claude.aaaa")
	bad.Problem = "machine is missing"
	res, err := resolveOwn(t, "0mcp:vm701", nil, gone, bad)
	if !errors.Is(err, ErrNoOwner) {
		t.Fatalf("err = %v", err)
	}
	if len(res.Ignored) != 1 || !strings.Contains(res.Ignored[0].Why, "does not parse") {
		t.Errorf("the malformed record is listed and the expired one is not: %+v", res.Ignored)
	}
	unrelated := rec("c000000001", "service:gateway", "imac", "imac/claude.aaaa")
	unrelated.Problem = "machine is missing"
	if res, _ := resolveOwn(t, "0mcp:vm701", nil, unrelated); len(res.Ignored) != 0 {
		t.Errorf("a record about another subject is not this lookup's business: %+v", res.Ignored)
	}
}

func TestOwnerAClaimForSomeoneElseNeedsATrustedDeclarer(t *testing.T) {
	forOther := rec("a000000001", "0mcp:vm701", "imac", "reg/claude.aaaa") // reg says imac owns it
	res, err := resolveOwn(t, "0mcp:vm701", nil, forOther)
	if !errors.Is(err, ErrNoOwner) || len(res.Ignored) != 1 || !strings.Contains(res.Ignored[0].Why, "aivoicebus owner trust 'reg/claude.*'") {
		t.Fatalf("an untrusted third-party claim: %v, %+v", err, res.Ignored)
	}
	res, err = resolveOwn(t, "0mcp:vm701", []string{"reg/claude.*"}, forOther)
	if err != nil || res.Record.Machine != "imac" || !res.Pinned {
		t.Fatalf("a trusted declarer's claim: %+v, %v", res, err)
	}
	if _, err = resolveOwn(t, "0mcp:vm701", []string{"reg2/claude.*", "*/codex.*"}, forOther); !errors.Is(err, ErrNoOwner) {
		t.Errorf("a pin for other hosts must not trust reg: %v", err)
	}
	self := rec("b000000001", "0mcp:vm701", "imac", "imac/claude.bbbb") // a claim for yourself needs no pin
	res, err = resolveOwn(t, "0mcp:vm701", nil, self)
	if err != nil || res.Pinned || res.Record.Machine != "imac" {
		t.Errorf("a self-declared claim: %+v, %v", res, err)
	}
}

func TestOwnerATrustedDeclarerOutranksEveryOtherClaim(t *testing.T) {
	trusted := rec("a000000001", "0mcp:*", "hyper", "hyper/claude.aaaa")
	squat := rec("b000000001", "0mcp:vm701", "evil", "evil/claude.bbbb") // more specific, newer, unpinned
	squat.At = ownerNow
	res, err := resolveOwn(t, "0mcp:vm701", []string{"hyper/claude.*"}, trusted, squat)
	if err != nil || res.Record.Machine != "hyper" || !res.Pinned {
		t.Fatalf("a squatter's exact claim must not beat a trusted family record: %+v, %v", res.Record, err)
	}
	if len(res.Ignored) != 1 || res.Ignored[0].Record.Machine != "evil" || !strings.Contains(res.Ignored[0].Why, "never overrides") {
		t.Errorf("the squatter is listed as outranked: %+v", res.Ignored)
	}
	// without the pin the specific claim wins, as a plain claim for yourself does
	res, err = resolveOwn(t, "0mcp:vm701", nil, trusted, squat)
	if err != nil || res.Record.Machine != "evil" {
		t.Errorf("unpinned, the most specific wins: %+v, %v", res.Record, err)
	}
}

func TestOwnerSameDeclarerSupersedesItself(t *testing.T) {
	old := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
	moved := rec("b000000001", "0mcp:vm701", "air", "air/claude.bbbb")
	moved.By = "imac/claude.cccc" // a newer session of the same declaring machine names a new owner
	moved.At = ownerNow.Add(-time.Hour)
	res, err := resolveOwn(t, "0mcp:vm701", []string{"imac/claude.*"}, old, moved)
	if err != nil || res.Record.Machine != "air" {
		t.Fatalf("the declaring machine's newest record stands alone: %+v, %v", res.Record, err)
	}
	// the other way round: the older record by that machine must not win
	moved.At = ownerNow.Add(-100 * time.Hour)
	res, err = resolveOwn(t, "0mcp:vm701", []string{"imac/claude.*"}, old, moved)
	if err != nil || res.Record.Machine != "imac" {
		t.Errorf("newest by time: %+v, %v", res.Record, err)
	}
}

func TestOwnerTwoDifferentOwnersAreNeverGuessed(t *testing.T) {
	a := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
	b := rec("b000000001", "0mcp:vm701", "air", "air/claude.bbbb")
	_, err := resolveOwn(t, "0mcp:vm701", nil, a, b)
	if !errors.Is(err, ErrOwnerConflict) || !strings.Contains(err.Error(), "imac") || !strings.Contains(err.Error(), "air") || !strings.Contains(err.Error(), "aivoicebus owner trust") {
		t.Errorf("unpinned conflict: %v", err)
	}
	_, err = resolveOwn(t, "0mcp:vm701", []string{"imac/claude.*", "air/claude.*"}, a, b)
	if !errors.Is(err, ErrOwnerConflict) || !strings.Contains(err.Error(), "ask the declarers") {
		t.Errorf("a conflict between trusted declarers: %v", err)
	}
	// a pin on one of them decides it
	res, err := resolveOwn(t, "0mcp:vm701", []string{"imac/claude.*"}, a, b)
	if err != nil || res.Record.Machine != "imac" {
		t.Errorf("the trusted one wins: %+v, %v", res.Record, err)
	}
	// two declarers that agree are one owner; the same machine with other needs is another
	b2 := rec("c000000001", "0mcp:vm701", "imac", "imac/claude.cccc")
	b2.By = "air/claude.dddd" // air also says imac
	if res, err := resolveOwn(t, "0mcp:vm701", []string{"air/claude.*", "imac/claude.*"}, a, b2); err != nil || res.Record.Machine != "imac" {
		t.Errorf("two declarers naming one owner: %v", err)
	}
	b3 := b2
	b3.Needs = []string{"role:pve"}
	if _, err := resolveOwn(t, "0mcp:vm701", []string{"air/claude.*", "imac/claude.*"}, a, b3); !errors.Is(err, ErrOwnerConflict) {
		t.Errorf("one machine with different needs is two owners: %v", err)
	}
}

func TestOwnerADeclarerThatPassesForAnotherIsIgnored(t *testing.T) {
	r := rec("a000000001", "0mcp:vm701", "imac", "imac/clаude.aaaa") // a Cyrillic а among ASCII letters
	r.By = "imac/clаude.aaaa"
	res, err := resolveOwn(t, "0mcp:vm701", nil, r)
	if !errors.Is(err, ErrNoOwner) || len(res.Ignored) != 1 || !strings.Contains(res.Ignored[0].Why, "passes for another") {
		t.Errorf("a lookalike declarer: %v %+v", err, res.Ignored)
	}
}

func TestOwnerAllowsUrgentAndAttachmentsOnlyThroughAPin(t *testing.T) {
	self := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
	res, _ := resolveOwn(t, "0mcp:vm701", nil, self)
	if err := res.Allows(false, false); err != nil {
		t.Errorf("a plain message: %v", err)
	}
	for _, c := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		err := res.Allows(c[0], c[1])
		if err == nil || !strings.Contains(err.Error(), "aivoicebus owner trust 'imac/claude.*'") {
			t.Errorf("urgent=%v attach=%v on an unpinned record: %v", c[0], c[1], err)
		}
	}
	res, _ = resolveOwn(t, "0mcp:vm701", []string{"imac/claude.*"}, self)
	if err := res.Allows(true, true); err != nil {
		t.Errorf("a pinned record may carry urgent files: %v", err)
	}
}

func TestOwnerMachineMustHaveBeenHeardFrom(t *testing.T) {
	self := rec("a000000001", "0mcp:vm701", "imac", "imac/claude.aaaa")
	res, _ := resolveOwn(t, "0mcp:vm701", nil, self)
	roster := []RosterEntry{
		{Me: "imac/claude.old1", Seen: ownerNow.Add(-9 * 24 * time.Hour)},
		{Me: "IMAC/claude.new2", Seen: ownerNow.Add(-3 * 24 * time.Hour)},
		{Me: "imac2/claude.zzzz", Seen: ownerNow},
	}
	heard := OwnerHeardAt(roster, "imac")
	if !heard.Equal(ownerNow.Add(-3 * 24 * time.Hour)) {
		t.Fatalf("OwnerHeardAt = %v: the newest session of the machine, matched without case, and not a machine with the same prefix", heard)
	}
	if err := res.MachineProblem(heard, ownerNow); err != nil {
		t.Errorf("heard 3d ago: %v", err)
	}
	for name, h := range map[string]time.Time{"never": {}, "8 days": ownerNow.Add(-8 * 24 * time.Hour)} {
		err := res.MachineProblem(h, ownerNow)
		if err == nil || !strings.Contains(err.Error(), "aivoicebus owner rm") || !strings.Contains(err.Error(), "imac") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if !OwnerHeardAt(nil, "imac").IsZero() {
		t.Error("an empty roster has heard nobody")
	}
}

func TestOwnerLineShowsWhoAndHowFarToTrustIt(t *testing.T) {
	r := rec("a000000001", "0mcp:*", "imac", "imac/claude.aaaa")
	r.Needs, r.Person = []string{"role:pve"}, "florin"
	res, err := resolveOwn(t, "0mcp:vm701", nil, r)
	if err != nil {
		t.Fatal(err)
	}
	line := res.Line(ownerNow)
	for _, want := range []string{"owner:0mcp:vm701 -> imac needs role:pve (florin) via 0mcp:*", "record by imac/claude.aaaa", "72h ago", "signed", "not pinned", "expires in 648h"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	res.Pinned = true
	if line := res.Line(ownerNow); !strings.Contains(line, ", pinned,") || strings.Contains(line, "not pinned") {
		t.Errorf("pinned line: %q", line)
	}
	if strings.ContainsAny(line, "\n\r\x00") {
		t.Errorf("a line is one line: %q", line)
	}
}
