package outbox

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Failures a script can tell apart (the CLI maps them to exit codes). They wrap nothing: the
// text is the whole message, the kind is only for errors.Is.
var (
	ErrNotFound  = errors.New("not found")
	ErrAmbiguous = errors.New("ambiguous")
	ErrUsage     = errors.New("usage")
)

// kindError is a message with a kind that errors.Is can match.
type kindError struct {
	text string
	kind error
}

func (e *kindError) Error() string        { return e.text }
func (e *kindError) Is(target error) bool { return target == e.kind }

// NotFoundf is an error that reads as text and matches ErrNotFound.
func NotFoundf(format string, a ...any) error {
	return &kindError{fmt.Sprintf(format, a...), ErrNotFound}
}

// Usagef is an error that reads as text and matches ErrUsage: the command was used wrongly, and
// running it again unchanged will not help.
func Usagef(format string, a ...any) error {
	return &kindError{fmt.Sprintf(format, a...), ErrUsage}
}

const (
	// MinIDPrefix is the shortest abbreviation accepted for reading (thread, ack, --re): fewer
	// characters match too much to trust a typo-prone hand with.
	MinIDPrefix = 4
	// MinRetractPrefix is the shortest for taking a message back, which is not undone by reading
	// again: peers choose the ids of what they send, so a short prefix is not an identity.
	MinRetractPrefix = 6
	// listedCandidates is how many matches an ambiguity error names.
	listedCandidates = 5
)

// idCand is an id a prefix may name, and who wrote it (shown whenever a prefix is resolved, so
// a prefix a peer planted is seen for what it is).
type idCand struct {
	id, from string
	at       time.Time
	to       string
}

// IDResolver turns what a person typed (a full message id, or the start of one) into a full id.
// The candidates are the ids this session can see without any new endpoint: its inbox (read and
// unread) and what this machine sent lately (sent.json). They are fetched at most once, and only
// when something shorter than a full id is given. Say, if set, hears one line for every
// abbreviation that was resolved.
type IDResolver struct {
	Say    io.Writer
	c      *client.Client
	me     string
	cands  []idCand
	loaded bool
	err    error // why the inbox could not be read, if it could not
}

// NewIDResolver is a resolver for session me reading through c.
func NewIDResolver(c *client.Client, me string) *IDResolver { return &IDResolver{c: c, me: me} }

// Resolve returns the full id that given names. A full id is returned as typed (lower-cased)
// without asking anyone. An abbreviation must match exactly one id: none is ErrNotFound, several
// is ErrAmbiguous, and both say what to do next. A malformed or too short one is ErrUsage.
func (r *IDResolver) Resolve(given string) (string, error) {
	id, err := shape(given, MinIDPrefix)
	if err != nil || len(id) == 10 {
		return id, err
	}
	if !r.loaded {
		r.load()
	}
	hit, err := resolveAmong(id, r.cands)
	if errors.Is(err, ErrNotFound) && r.err != nil {
		return "", r.err // nothing matched because the inbox could not be read: say that, not "no such id"
	}
	if err != nil {
		return "", err
	}
	r.say(id, hit)
	return hit.id, nil
}

// ResolveSent is Resolve for taking a message back: only what this machine sent (never the
// inbox, where a peer could have planted an id), and at least MinRetractPrefix characters. It
// returns the remembered message too, so the caller can say what it is about to act on.
func (r *IDResolver) ResolveSent(given string) (id, to string, at time.Time, err error) {
	id, err = shape(given, MinRetractPrefix)
	if err != nil {
		return "", "", time.Time{}, err
	}
	var sent []idCand
	for _, e := range readSent(clock())[msg.HostOf(r.me)] {
		sent = append(sent, idCand{id: e.ID, from: e.From, to: e.To, at: e.At})
	}
	if len(id) == 10 { // a full id needs no memory, but when it is there its target can be shown
		for _, c := range sent {
			if c.id == id {
				return id, c.to, c.at, nil
			}
		}
		return id, "", time.Time{}, nil
	}
	hit, err := resolveAmong(id, sent)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", "", time.Time{}, NotFoundf("no message id starting with %q among what this machine sent in the last %s (retract looks nowhere else): give the full id", id, sentWindowText)
		}
		return "", "", time.Time{}, err
	}
	r.say(id, hit)
	return hit.id, hit.to, hit.at, nil
}

// Expand is Resolve for an abbreviation and the identity function for anything else, so a
// caller with its own validation of full ids (an MCP tool) keeps its messages for bad input.
func (r *IDResolver) Expand(given string) (string, error) {
	if !IsAbbreviation(given) {
		return given, nil
	}
	return r.Resolve(given)
}

// ExpandSent is ResolveSent for an abbreviation of at least minLen characters (an MCP tool asks
// for more than the CLI), and the identity function for anything else.
func (r *IDResolver) ExpandSent(given string, minLen int) (string, error) {
	if !IsAbbreviation(given) {
		return given, nil
	}
	if id := strings.ToLower(strings.TrimSpace(given)); len(id) < minLen {
		return "", Usagef("%q is too short to take a message back: give at least %d characters or the full id", id, minLen)
	}
	id, _, _, err := r.ResolveSent(given)
	return id, err
}

func (r *IDResolver) say(prefix string, hit idCand) {
	if r.Say != nil {
		fmt.Fprintf(r.Say, "resolved %s -> %s from %s\n", prefix, hit.id, view.OneLine(hit.from, 60))
	}
}

// IsAbbreviation reports whether given is shorter than a full id but shaped like the start of
// one (at least MinIDPrefix hex digits), which Resolve can expand.
func IsAbbreviation(given string) bool {
	id := strings.ToLower(strings.TrimSpace(given))
	return hexPrefix(id) && len(id) >= MinIDPrefix && len(id) < 10
}

// shape lower-cases given and checks it is a full id or a hex prefix of at least min characters.
func shape(given string, min int) (string, error) {
	id := strings.ToLower(strings.TrimSpace(given))
	switch {
	case msg.ValidRetractID(id):
		return id, nil
	case !hexPrefix(id):
		return "", Usagef("%q is not a message id (ten lowercase hex digits, or the first %d or more of them)", view.OneLine(given, 40), min)
	case len(id) < min:
		return "", Usagef("%q is too short to name a message: give at least %d characters", id, min)
	}
	return id, nil
}

// load gathers the candidates. A failing inbox read leaves the sent memory's ids, and is
// remembered so that a miss can be reported as what it is.
func (r *IDResolver) load() {
	r.loaded = true
	for _, e := range readSent(clock())[msg.HostOf(r.me)] {
		r.cands = append(r.cands, idCand{id: e.ID, from: e.From})
	}
	if len(r.c.Labels) == 0 {
		r.c.Labels = identity.Labels("") // this read declares the session's labels: do not blank them
	}
	ms, err := r.c.Inbox(r.me, true)
	if err != nil {
		r.err = err
	}
	for _, m := range ms {
		r.cands = append(r.cands, idCand{id: m.ID, from: m.From})
		if msg.ValidRetractID(m.Thread) {
			r.cands = append(r.cands, idCand{id: m.Thread, from: m.From})
		}
	}
	slices.SortStableFunc(r.cands, func(a, b idCand) int { return strings.Compare(a.id, b.id) })
	r.cands = slices.CompactFunc(r.cands, func(a, b idCand) bool { return a.id == b.id })
}

// resolveAmong picks the one candidate whose id starts with prefix.
func resolveAmong(prefix string, cands []idCand) (idCand, error) {
	var hits []idCand
	for _, c := range cands {
		if strings.HasPrefix(c.id, prefix) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 0:
		return idCand{}, NotFoundf("no message id starting with %q among this session's inbox and what this machine sent in the last %s: give the full id (`aivoicebus inbox --all` lists ids)", prefix, sentWindowText)
	case 1:
		return hits[0], nil
	}
	return idCand{}, &kindError{fmt.Sprintf("%q matches %d messages: %s; give more characters", prefix, len(hits), listCandidates(hits)), ErrAmbiguous}
}

// listCandidates names the first few as "id (from sender)", and how many more there are.
func listCandidates(hits []idCand) string {
	var parts []string
	for _, h := range hits[:min(len(hits), listedCandidates)] {
		parts = append(parts, h.id+" (from "+view.OneLine(h.from, 60)+")")
	}
	s := strings.Join(parts, ", ")
	if more := len(hits) - len(parts); more > 0 {
		s += fmt.Sprintf(" and %d more", more)
	}
	return s
}

func hexPrefix(s string) bool {
	if s == "" || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
