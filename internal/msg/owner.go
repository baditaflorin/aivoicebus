package msg

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Owner records: who answers for a subject ("0mcp:vm701", "service:gateway", "domain:example.com").
//
// A record is a lease (lease.go) on the key "owner:SUBJECT" whose note says machine=M [needs=...]
// [person=NAME]. Nothing on the nodes is new: a lease is already signed, replicated, retractable by
// any session on the machine that wrote it, and gone after at most MaxTTL, so an owner who stops
// renewing drops out by itself. Only the reader is new, and it does not use the lease rule ("the
// earliest holder wins"), which would let whoever claimed a subject first keep it. Instead:
//   - a record counts only if its signature verifies and the machine's key is the pinned one;
//   - the machine that declares it must be the owner it names (a claim for yourself), or the sender
//     must have trusted the declarer (`owner trust`): a claim for someone else is shown, not used;
//   - a record from a declarer the sender trusts outranks every other claim to the same subject, and
//     is the only kind used for an urgent message, a file or a log link;
//   - of the records left, the most specific subject wins (an exact one over a declared glob such as
//     "0mcp:*"), and two different owners at the same level are an error naming both, never a guess.
//
// A machine is not an owner by itself (one laptop carries many sessions), so a record can say which
// of its sessions answer with needs, in the vocabulary --needs already has (role:pve, repo:o/x).
// person is a label for people: routing to the person is by machine (<machine>/human).
//
// A node that holds leases (LeaseSince) stores and replicates the records unchanged, so the wire
// format does not change. A client older than this reads them as leases whose key starts "owner:".
const (
	OwnerPrefix = "owner:"
	// MaxOwnerSubject keeps OwnerPrefix and the subject within one lease key.
	MaxOwnerSubject = MaxLeaseKey - len(OwnerPrefix)
	MaxOwnerPerson  = 40
	MaxOwnerNote    = 400
	// OwnerTTL is how long a record lasts when nobody says: as long as a lease can.
	OwnerTTL = MaxTTL
	// OwnerHeard is how recently some session on the owner's machine must have been heard from for a
	// message to be sent to it: a retired machine's person never reads what is sent there.
	OwnerHeard = 7 * 24 * time.Hour
	// ownerLiteral is how many literal characters a declared glob keeps in front of its first '*',
	// so "*" or "v*" cannot claim every subject.
	ownerLiteral = 3
)

// MaxLeaseKey is the longest lease key (see ValidLeaseKey).
const MaxLeaseKey = 120

// ErrNoOwner and ErrOwnerConflict are what ResolveOwner answers when nothing, or two different owners, fit.
var (
	ErrNoOwner       = errors.New("no owner is recorded")
	ErrOwnerConflict = errors.New("two owners are recorded")
)

// IsOwnerKey reports whether a lease key is in the owner namespace.
func IsOwnerKey(key string) bool { return strings.HasPrefix(key, OwnerPrefix) }

// OwnerKey is the lease key of a subject. A lease key has no '*', so the '*' of a family ("0mcp:*") is
// written '@' in the key ("owner:0mcp:@"), which a subject never holds (ValidOwnerLookup).
func OwnerKey(subject string) string { return OwnerPrefix + strings.ReplaceAll(subject, "*", "@") }

// OwnerSubjectOf reads a send target of the form owner:SUBJECT. ok is false for any other target; the
// subject is lower case and is not checked (see ValidOwnerLookup).
func OwnerSubjectOf(target string) (string, bool) {
	t := strings.TrimSpace(target)
	if len(t) < len(OwnerPrefix) || !strings.EqualFold(t[:len(OwnerPrefix)], OwnerPrefix) {
		return "", false
	}
	return strings.ToLower(t[len(OwnerPrefix):]), true
}

const ownerSubjectChars = "abcdefghijklmnopqrstuvwxyz0123456789._:/+-"

// ValidOwnerLookup reports why s cannot be looked up, or nil: lower case letters, digits and . _ : / + -,
// 1 to MaxOwnerSubject characters. The names fleet-where and fleet-sh use are of this shape
// ("0mcp:vm701", "alias:0mcp_docker_build", "service:go-fleet-backup"); aivoicebus compares them as text.
func ValidOwnerLookup(s string) error {
	switch {
	case s == "" || len(s) > MaxOwnerSubject:
		return fmt.Errorf("a subject is 1 to %d characters, like 0mcp:vm701 or service:gateway", MaxOwnerSubject)
	case strings.Trim(s, ownerSubjectChars) != "":
		return errors.New("a subject uses lower case letters, digits and . _ : / + - only, like 0mcp:vm701 or service:gateway")
	}
	return nil
}

// ValidOwnerSubject is ValidOwnerLookup for a subject a record is written for: it may also hold '*', which
// declares a default for a family ("0mcp:*"), but only behind at least three literal characters.
func ValidOwnerSubject(s string) error {
	switch {
	case s == "" || len(s) > MaxOwnerSubject:
		return fmt.Errorf("a subject is 1 to %d characters, like 0mcp:vm701 or service:gateway", MaxOwnerSubject)
	case strings.Trim(s, ownerSubjectChars+"*") != "":
		return errors.New("a subject uses lower case letters, digits and . _ : / + - (and * for a family) only, like 0mcp:vm701 or service:gateway")
	}
	if i := strings.IndexByte(s, '*'); i >= 0 && i < ownerLiteral {
		return fmt.Errorf("a subject with * keeps at least %d literal characters in front of it (0mcp:*), so it cannot claim everything", ownerLiteral)
	}
	return nil
}

// ValidOwnerMachine reports why m cannot be the machine of an owner record, or nil: one machine name, as
// a target writes it, that does not pass for another.
func ValidOwnerMachine(m string) error {
	switch {
	case m == "" || len(m) > MaxIdentity || hasControl(m):
		return errors.New("machine is the owner's machine name, such as the first column of `aivoicebus fleet`")
	case strings.ContainsAny(m, "/@*:, \t"):
		return errors.New("machine is one machine name, without / @ * : or spaces (the first column of `aivoicebus fleet`)")
	case m != strings.ToLower(m):
		return errors.New("machine is lower case, as `aivoicebus fleet` writes it")
	case m == "all" || m == SystemHost || m == "human":
		return fmt.Errorf("%q is not a machine: name the owner's machine, the first column of `aivoicebus fleet`", m)
	}
	if why := TargetProblem(m); why != "" {
		return fmt.Errorf("the machine name %s: use its plain ASCII name", why)
	}
	return nil
}

// ValidOwnerPerson reports whether p can be a record's person: one word, 1 to MaxOwnerPerson characters.
func ValidOwnerPerson(p string) bool {
	if p == "" || len(p) > MaxOwnerPerson {
		return false
	}
	for i, r := range p {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || (i > 0 && (r == '.' || r == '_' || r == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// OwnerFields is what a record says about the owner.
type OwnerFields struct {
	Machine string   // required
	Needs   []string // which of its sessions answer (ParseNeeds); none: any session there
	Person  string   // a label for people; routing to the person is by machine
}

// Note is the lease note of a record: machine=M needs=a,b person=P.
func (f OwnerFields) Note() string {
	parts := []string{"machine=" + f.Machine}
	if len(f.Needs) > 0 {
		parts = append(parts, "needs="+strings.Join(f.Needs, ","))
	}
	if f.Person != "" {
		parts = append(parts, "person="+f.Person)
	}
	return strings.Join(parts, " ")
}

// Check reports why the fields cannot be written as a record, or nil.
func (f OwnerFields) Check() error {
	if err := ValidOwnerMachine(f.Machine); err != nil {
		return err
	}
	if f.Person != "" && !ValidOwnerPerson(f.Person) {
		return fmt.Errorf("person is one word of letters, digits, . _ - (at most %d characters), such as a first name", MaxOwnerPerson)
	}
	if len(f.Needs) > MaxNeeds {
		return fmt.Errorf("needs asks for %d capabilities; at most %d", len(f.Needs), MaxNeeds)
	}
	return nil
}

// ParseOwnerNote reads the note of an owner record: words of the form key=value, in any order; a word
// without '=' and a key it does not know are ignored, so a later release can add fields. A known key
// twice, an unusable value or a missing machine is an error: the record is shown and never used.
func ParseOwnerNote(note string) (OwnerFields, error) {
	var f OwnerFields
	if len(note) > MaxOwnerNote {
		return f, fmt.Errorf("the note is longer than %d characters", MaxOwnerNote)
	}
	seen := map[string]bool{}
	for _, w := range strings.Fields(note) {
		k, v, ok := strings.Cut(w, "=")
		if !ok || (k != "machine" && k != "needs" && k != "person") {
			continue
		}
		if seen[k] {
			return f, fmt.Errorf("%s is given twice", k)
		}
		seen[k] = true
		switch k {
		case "machine":
			f.Machine = v
		case "person":
			f.Person = v
		case "needs":
			ns, err := ParseNeeds(v)
			if err != nil {
				return f, fmt.Errorf("needs: %v", err)
			}
			f.Needs = ns
		}
	}
	if err := f.Check(); err != nil {
		return f, err
	}
	return f, nil
}

// OwnerRecord is one live owner lease, read.
type OwnerRecord struct {
	Subject string    `json:"subject"` // lower case; may hold '*'
	Machine string    `json:"machine,omitempty"`
	Needs   []string  `json:"needs,omitempty"`
	Person  string    `json:"person,omitempty"`
	By      string    `json:"by"` // the session that wrote it; only its machine matters
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	Until   time.Time `json:"until"`
	// Verified is the signature verdict of the record's message (VerifiedSigned and the others), set by
	// the reader that fetched it; empty when it could not be checked.
	Verified string `json:"verified,omitempty"`
	// Problem says why the record cannot be used at all (a note or subject that does not parse).
	Problem string `json:"problem,omitempty"`
}

// OwnerOf reads a lease as an owner record. ok is false for a lease in another namespace. A lease in
// this one that does not parse comes back with ok true and Problem set, so it can be listed.
func OwnerOf(l Lease) (OwnerRecord, bool) {
	subject, found := strings.CutPrefix(l.Key, OwnerPrefix)
	if !found {
		return OwnerRecord{}, false
	}
	subject = strings.ReplaceAll(subject, "@", "*") // see OwnerKey
	r := OwnerRecord{Subject: subject, By: l.By, ID: l.ID, At: l.At, Until: l.Until}
	if err := ValidOwnerSubject(subject); err != nil {
		r.Problem = err.Error()
		return r, true
	}
	f, err := ParseOwnerNote(l.Note)
	if err != nil {
		r.Problem = err.Error()
		return r, true
	}
	r.Machine, r.Needs, r.Person = f.Machine, f.Needs, f.Person
	return r, true
}

// ParseOwnerPins reads the owner-trust file: one "host/agent" pattern per line ("#" starts a comment),
// the same shape as a role pin (ValidRolePin). A line that is not a valid pin is skipped, so one typo does
// not hide the others.
func ParseOwnerPins(text string) []string {
	var pins []string
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		p := strings.ToLower(strings.TrimSpace(line))
		if p != "" && ValidRolePin(p) == nil && !slices.Contains(pins, p) {
			pins = append(pins, p)
		}
	}
	return pins
}

// OwnerTrustHint is the command that trusts the machine and tool of identity by.
func OwnerTrustHint(by string) string {
	host, agent, _ := strings.Cut(strings.ToLower(by), "/")
	tool, _, _ := strings.Cut(agent, ".")
	return fmt.Sprintf("aivoicebus owner trust '%s/%s.*'", SafeName(host), SafeName(tool))
}

// OwnerIgnored is a record that was not used, and why.
type OwnerIgnored struct {
	Record OwnerRecord
	Why    string
}

// OwnerResolution is what a lookup came to.
type OwnerResolution struct {
	Subject string
	Record  OwnerRecord // the owner; zero when the lookup failed
	Pinned  bool        // its declarer matches the sender's owner-trust patterns
	Ignored []OwnerIgnored
}

// ownerWhyNot is why r cannot be used by a sender with these pins, "" when it can. Whether the declarer
// is trusted is a second question (declarerPinned): a claim for yourself is used without a pin.
func ownerWhyNot(r OwnerRecord, pins []string) string {
	switch {
	case r.Problem != "":
		return "does not parse: " + r.Problem
	case !ValidIdentity(r.By) || SenderProblem(r.By) != "" || NameProblem(r.By) != "":
		return "was declared by a name that passes for another"
	}
	switch r.Verified {
	case VerifiedSigned:
	case VerifiedUnsigned:
		return "is not signed"
	case VerifiedBad:
		return "has a signature that does not verify"
	case VerifiedChanged:
		return "was signed with a key other than the one pinned for " + SafeName(HostOf(r.By)) + " (KEY CHANGED: `aivoicebus keys`)"
	default:
		return "could not be checked: its signature was not read"
	}
	if HostOf(r.By) != r.Machine && !declarerPinned(r, pins) {
		return fmt.Sprintf("names %s as owner but was declared by %s, whom you do not trust (%s)", SafeName(r.Machine), SafeName(r.By), OwnerTrustHint(r.By))
	}
	return ""
}

func declarerPinned(r OwnerRecord, pins []string) bool {
	return slices.ContainsFunc(pins, func(p string) bool { return pinMatches(p, r.By) })
}

// ownerSpecificity ranks subjects: an exact one beats any glob, and a glob is the better the more literal
// characters it has.
func ownerSpecificity(subject string) int {
	if !strings.Contains(subject, "*") {
		return 1 << 20
	}
	return len(subject) - strings.Count(subject, "*")
}

func ownerKey(r OwnerRecord) string { return r.Machine + "|" + strings.Join(r.Needs, ",") }

// ResolveOwner finds the owner of subject among recs (every live owner record the nodes hold that might
// match, verified by the caller), for a sender that trusts pins. Not used records are listed in the result,
// with why. ErrNoOwner or ErrOwnerConflict (wrapped, with what to do) when it did not come to one owner.
func ResolveOwner(subject string, recs []OwnerRecord, pins []string, now time.Time) (OwnerResolution, error) {
	res := OwnerResolution{Subject: subject}
	var pinned, plain []OwnerRecord
	for _, r := range recs {
		if ValidOwnerSubject(r.Subject) != nil || !MatchKey(r.Subject, subject) || !r.Until.After(now) {
			continue // not about this subject (a record whose subject is unusable is only listed by `owner list`), or over
		}
		if why := ownerWhyNot(r, pins); why != "" {
			res.Ignored = append(res.Ignored, OwnerIgnored{r, why})
		} else if declarerPinned(r, pins) {
			pinned = append(pinned, r)
		} else {
			plain = append(plain, r)
		}
	}
	use := plain
	if len(pinned) > 0 {
		use, res.Pinned = pinned, true
		for _, r := range plain {
			res.Ignored = append(res.Ignored, OwnerIgnored{r, "a declarer you trust names an owner for this subject, and an unpinned claim never overrides it"})
		}
	}
	// One record per declaring machine and subject: its newest. A machine that moves a subject writes a new
	// record, and the old one it did not retract must not make a conflict with it.
	newest := func(a, b OwnerRecord) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	}
	slices.SortStableFunc(use, newest)
	var fresh []OwnerRecord
	seen := map[string]bool{}
	for _, r := range use {
		if k := HostOf(r.By) + "|" + r.Subject; !seen[k] {
			seen[k] = true
			fresh = append(fresh, r)
		}
	}
	best := -1
	for _, r := range fresh {
		best = max(best, ownerSpecificity(r.Subject))
	}
	var tier []OwnerRecord
	for _, r := range fresh {
		if ownerSpecificity(r.Subject) == best {
			tier = append(tier, r)
		}
	}
	if len(tier) == 0 {
		why := ""
		if n := len(res.Ignored); n > 0 {
			why = fmt.Sprintf(" (%d record(s) were not used: `aivoicebus owner get %s` says why)", n, SafeName(subject))
		}
		return res, fmt.Errorf("%w for %s%s: whoever answers for it records itself with `aivoicebus owner set %s` (`aivoicebus owner list` shows what is recorded)", ErrNoOwner, SafeName(subject), why, SafeName(subject))
	}
	var owners []OwnerRecord
	for _, r := range tier {
		if !slices.ContainsFunc(owners, func(o OwnerRecord) bool { return ownerKey(o) == ownerKey(r) }) {
			owners = append(owners, r)
		}
	}
	if len(owners) > 1 {
		var parts []string
		for _, o := range owners[:min(len(owners), 3)] {
			p := SafeName(o.Machine)
			if len(o.Needs) > 0 {
				p += " (needs " + SafeName(strings.Join(o.Needs, ",")) + ")"
			}
			parts = append(parts, fmt.Sprintf("%s, declared by %s %s ago", p, SafeName(o.By), SpanWords(now.Sub(o.At))))
		}
		next := "pin the declarer you trust: " + OwnerTrustHint(owners[0].By)
		if res.Pinned {
			next = "ask the declarers which is right, then one of them runs `aivoicebus owner rm " + SafeName(subject) + "` or `aivoicebus owner set`"
		}
		return res, fmt.Errorf("%w for %s: %s; an owner is never guessed between claims, so %s", ErrOwnerConflict, SafeName(subject), strings.Join(parts, " and "), next)
	}
	res.Record = owners[0]
	return res, nil
}

// Allows reports whether the record may be used for a message that is urgent or carries a file or a log
// link: only one from a declarer the sender trusts (anyone with the token can write a record).
func (r OwnerResolution) Allows(urgent, attach bool) error {
	if r.Pinned || (!urgent && !attach) {
		return nil
	}
	return fmt.Errorf("owner:%s names %s, but its declarer %s is not one you trust, and an owner record nobody pinned is never used for an urgent message or a file or log link (anyone with the token can write one). Pin the declarer: %s",
		SafeName(r.Subject), SafeName(r.Record.Machine), SafeName(r.Record.By), OwnerTrustHint(r.Record.By))
}

// OwnerHeardAt is when a session on machine was last seen in the roster; zero when none ever was.
func OwnerHeardAt(roster []RosterEntry, machine string) time.Time {
	var at time.Time
	for _, e := range roster {
		if HostOf(e.Me) == machine && e.Seen.After(at) {
			at = e.Seen
		}
	}
	return at
}

// MachineProblem is why a message for the record's machine would go nowhere (nobody there was heard from
// within OwnerHeard), as an error that says what to do, or nil. heard is OwnerHeardAt.
func (r OwnerResolution) MachineProblem(heard, now time.Time) error {
	if !heard.IsZero() && now.Sub(heard) <= OwnerHeard {
		return nil
	}
	seen := "has never been seen"
	if !heard.IsZero() {
		seen = fmt.Sprintf("was last heard from %s ago", SpanWords(now.Sub(heard)))
	}
	return fmt.Errorf("owner:%s names %s, which %s (nothing is sent to a machine nobody has been heard from in %s): its record by %s expires in %s; ask the user who owns it now, and the machine's person can renew it or remove it with `aivoicebus owner rm %s`",
		SafeName(r.Subject), SafeName(r.Record.Machine), seen, SpanWords(OwnerHeard), SafeName(r.Record.By), SpanWords(r.Record.Until.Sub(now)), SafeName(r.Record.Subject))
}

// Line is the one line shown with a resolved owner: who it came to and how far to trust it.
func (r OwnerResolution) Line(now time.Time) string {
	rec := r.Record
	var b strings.Builder
	fmt.Fprintf(&b, "owner:%s -> %s", SafeName(r.Subject), SafeName(rec.Machine))
	if len(rec.Needs) > 0 {
		b.WriteString(" needs " + SafeName(strings.Join(rec.Needs, ",")))
	}
	if rec.Person != "" {
		b.WriteString(" (" + SafeName(rec.Person) + ")")
	}
	if rec.Subject != r.Subject {
		b.WriteString(" via " + SafeName(rec.Subject))
	}
	trust := "not pinned"
	if r.Pinned {
		trust = "pinned"
	}
	fmt.Fprintf(&b, ": record by %s, %s ago, signed, %s, expires in %s", SafeName(rec.By), SpanWords(now.Sub(rec.At)), trust, SpanWords(rec.Until.Sub(now)))
	return b.String()
}
