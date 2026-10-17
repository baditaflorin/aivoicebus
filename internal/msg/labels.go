package msg

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Labels describe what a session is working on: its repo and branch, its tool,
// its machine. A session declares them with every inbox read; a node matches a
// message's selector against them at read time, so nothing about labels is
// replicated or stored with messages.
//
// Labels are routing hints, not access control. Whoever holds the shared token
// can claim any identity and read any message today; claiming labels adds
// nothing to that.
type Labels map[string]string

// Limits keep labels and selectors small and safe to put in a URL.
const (
	MaxLabels     = 12
	MaxLabelValue = 64
)

// ParamLabels is the inbox query parameter that carries a session's labels.
const ParamLabels = "labels"

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,23}$`)

// ValidKey reports whether k is an acceptable label key.
func ValidKey(k string) bool { return keyRe.MatchString(k) }

// CleanValue makes s safe as a label value: lowercase, only letters, digits and
// . _ / + - (anything else becomes '-'), at most MaxLabelValue characters, no
// leading or trailing '-'. It returns "" if nothing usable is left.
func CleanValue(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '/' || r == '+' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= MaxLabelValue {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

// ParseLabels reads "key=value,key=value". Pairs that are malformed or whose
// key is not valid are dropped, values are cleaned, and at most MaxLabels pairs
// are kept (in sorted key order), so hostile input cannot grow without bound.
func ParseLabels(s string) Labels {
	l := Labels{}
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = CleanValue(v)
		if ok && ValidKey(k) && v != "" {
			l[k] = v
		}
	}
	return l.limited()
}

// limited keeps the first MaxLabels keys in sorted order.
func (l Labels) limited() Labels {
	if len(l) <= MaxLabels {
		return l
	}
	out := Labels{}
	for _, k := range l.keys()[:MaxLabels] {
		out[k] = l[k]
	}
	return out
}

func (l Labels) keys() []string {
	ks := make([]string, 0, len(l))
	for k := range l {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// String is "key=value,key=value" in sorted key order ("" for no labels).
func (l Labels) String() string {
	parts := make([]string, 0, len(l))
	for _, k := range l.keys() {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

// Overlay returns l with every label of over set on top.
func (l Labels) Overlay(over Labels) Labels {
	out := Labels{}
	for k, v := range l {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out.limited()
}

// RosterEntry is one session a node has heard from, with its declared labels.
type RosterEntry struct {
	Me     string    `json:"me"`
	Seen   time.Time `json:"seen"`
	Labels Labels    `json:"labels,omitempty"`
	// Title is what the session's tool last called it (see CleanTitle); empty when it declared none.
	Title string `json:"title,omitempty"`
	// Listening is set by the node a session is waiting on right now (in `aivoicebus wait` or
	// --wait-reply): a message to it is read within a second. Not gossiped; MergeRosters ORs it.
	Listening bool `json:"listening,omitempty"`
}

// MergeRosters combines the rosters several nodes reported into one: each session
// once, as last seen on any node, with the labels of its most recent report that
// had any (a node that knows a session only from a message it sent has none).
// A node learns a session's labels only when that session reads from this node,
// so one node's roster is a partial view.
func MergeRosters(lists ...[]RosterEntry) []RosterEntry {
	byMe := map[string]RosterEntry{}
	labelsAt := map[string]time.Time{}
	titleAt := map[string]time.Time{}
	for _, list := range lists {
		for _, e := range list {
			cur, ok := byMe[e.Me]
			if !ok {
				cur = RosterEntry{Me: e.Me}
			}
			if e.Seen.After(cur.Seen) {
				cur.Seen = e.Seen
			}
			if len(e.Labels) > 0 && !e.Seen.Before(labelsAt[e.Me]) {
				cur.Labels, labelsAt[e.Me] = e.Labels, e.Seen
			}
			if e.Title != "" && !e.Seen.Before(titleAt[e.Me]) {
				cur.Title, titleAt[e.Me] = e.Title, e.Seen
			}
			cur.Listening = cur.Listening || e.Listening
			byMe[e.Me] = cur
		}
	}
	out := make([]RosterEntry, 0, len(byMe))
	for _, e := range byMe {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Me < out[j].Me })
	return out
}

// Term is one condition of a selector. Positive (key=a|b): the label Key must be
// present and its value match any of Pats ('*' matches any run of characters).
// Negated (key!=a|b): the label must be absent or match none of them.
type Term struct {
	Key  string
	Pats []string
	Not  bool
}

// Selector addresses every session whose labels satisfy all its terms:
// "@repo=go-url-categorizer-api,tool=claude", "@repo=api|web", "@tool=claude,repo!=x".
type Selector []Term

// Limits for one selector term.
const MaxAlternatives = 8

// SelectorSince is the first release whose nodes understand alternatives (a|b)
// and negation (!=). A node that does not stores such a message but delivers it
// to nobody, so senders check before using the new forms. Every release of this project
// has them; the constant stays so a node from elsewhere can be told apart.
const SelectorSince = "0.1.0"

// IsSelector reports whether a message target is a selector.
func IsSelector(to string) bool { return strings.HasPrefix(to, "@") }

// ParseSelector parses "@key=pattern[,key=pattern...]". The errors say what is
// wrong, because they are shown to whoever mistyped the target.
func ParseSelector(to string) (Selector, error) {
	if !IsSelector(to) {
		return nil, errors.New("a selector starts with @")
	}
	body := strings.TrimPrefix(to, "@")
	if strings.TrimSpace(body) == "" {
		return nil, errors.New(`selector needs at least one key=value, e.g. @repo=myrepo`)
	}
	var sel Selector
	for _, term := range strings.Split(body, ",") {
		t, err := parseTerm(term)
		if err != nil {
			return nil, err
		}
		sel = append(sel, t)
		if len(sel) > MaxLabels {
			return nil, fmt.Errorf("a selector has at most %d terms", MaxLabels)
		}
	}
	return sel, nil
}

func parseTerm(term string) (Term, error) {
	term = strings.TrimSpace(term)
	k, p, not := "", "", false
	if a, b, ok := strings.Cut(term, "!="); ok && !strings.Contains(a, "=") {
		k, p, not = a, b, true
	} else if a, b, ok := strings.Cut(term, "="); ok {
		k, p = a, b
	} else {
		return Term{}, fmt.Errorf("selector term %q needs the form key=value", term)
	}
	k = strings.ToLower(strings.TrimSpace(k))
	switch {
	case p == "":
		return Term{}, fmt.Errorf("selector term %q needs the form key=value", term)
	case !ValidKey(k):
		return Term{}, fmt.Errorf("selector key %q is not valid (lowercase letters, digits, - and _, starting with a letter)", k)
	}
	pats := strings.Split(strings.ToLower(p), "|")
	if len(pats) > MaxAlternatives {
		return Term{}, fmt.Errorf("selector term %q has more than %d alternatives", term, MaxAlternatives)
	}
	for _, pat := range pats {
		if pat == "" || len(pat) > MaxLabelValue || strings.Trim(pat, "abcdefghijklmnopqrstuvwxyz0123456789._/+*-") != "" {
			return Term{}, fmt.Errorf("selector value %q may only use letters, digits, . _ / + - and * (at most %d characters), with | between alternatives", p, MaxLabelValue)
		}
	}
	return Term{Key: k, Pats: pats, Not: not}, nil
}

// Advanced reports whether the selector uses a form older nodes cannot read.
func (s Selector) Advanced() bool {
	for _, t := range s {
		if t.Not || len(t.Pats) > 1 {
			return true
		}
	}
	return false
}

// Match reports whether labels satisfy every term.
func (s Selector) Match(l Labels) bool {
	for _, t := range s {
		if t.holds(l) == t.Not {
			return false
		}
	}
	return len(s) > 0
}

// holds is whether the label is present and matches one of the alternatives; a
// negated term is satisfied when this is false.
func (t Term) holds(l Labels) bool {
	v, ok := l[t.Key]
	if !ok {
		return false
	}
	for _, p := range t.Pats {
		if glob(p, v) {
			return true
		}
	}
	return false
}

// String is the selector in its "@key=pattern,..." form.
func (s Selector) String() string {
	parts := make([]string, len(s))
	for i, t := range s {
		op := "="
		if t.Not {
			op = "!="
		}
		parts[i] = t.Key + op + strings.Join(t.Pats, "|")
	}
	return "@" + strings.Join(parts, ",")
}

// ExpandGroup replaces a leading "+name" term of a selector target with the
// selector the sender saved under that name: "@+infra,tool=claude" with
// infra="@repo=a|b" becomes "@repo=a|b,tool=claude". Groups are resolved when the
// message is sent, so nodes never need to know them.
func ExpandGroup(to string, groups map[string]string) (string, error) {
	body, found := strings.CutPrefix(to, "@+")
	if !found {
		return to, nil
	}
	name, rest, _ := strings.Cut(body, ",")
	name = strings.ToLower(strings.TrimSpace(name))
	sel, ok := groups[name]
	if !ok {
		return "", fmt.Errorf("no group named %q (groups live in ~/.config/aivoicebus/groups, one name=@selector per line)", name)
	}
	if _, err := ParseSelector(sel); err != nil {
		return "", fmt.Errorf("group %q is not a valid selector: %v", name, err)
	}
	if rest != "" {
		sel += "," + rest
	}
	return sel, nil
}

// ParseGroups reads a groups file: "name=@selector" per line, "#" starts a comment.
// Lines that are not a name with a valid selector are skipped, so one typo does
// not hide the other groups.
func ParseGroups(text string) map[string]string {
	g := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		name, sel, ok := strings.Cut(strings.TrimSpace(line), "=")
		name, sel = strings.ToLower(strings.TrimSpace(name)), strings.ToLower(strings.TrimSpace(sel))
		if _, err := ParseSelector(sel); ok && ValidKey(name) && err == nil {
			g[name] = sel
		}
	}
	return g
}

// ExpandHere replaces a leading "here" term of a selector target with the sender's
// machine: "@here,tool=claude" becomes "@machine=<machine>,tool=claude". "here" is
// resolved when the message is sent, because nodes do not know who is asking.
func ExpandHere(to string, own Labels) (string, error) {
	rest, found := strings.CutPrefix(to, "@here")
	if !found || (rest != "" && rest[0] != ',') {
		return to, nil
	}
	machine := own["machine"]
	if machine == "" {
		return "", errors.New("@here needs this session's machine label, which is not known here")
	}
	return "@machine=" + machine + rest, nil
}

// CapsLabel is the label a session advertises its capabilities in: free-form words
// joined with '+', conventionally ask (will answer questions), run (will run commands
// or tests for others) and read-only (will not edit anything). Nothing enforces them;
// they tell a sender whom to ask.
const CapsLabel = "caps"

// CleanCaps makes a capability list ("ask, run" or "ask+run") one label value: each word
// cleaned as a label value, duplicates dropped, sorted, joined with '+', and cut to
// whole words within MaxLabelValue. "" if nothing usable is left.
func CleanCaps(s string) string {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '+' || r == ' ' || r == '\t' })
	set := map[string]bool{}
	for _, w := range words {
		if w = CleanValue(w); w != "" {
			set[w] = true
		}
	}
	out := ""
	for _, w := range slices.Sorted(maps.Keys(set)) {
		next := w
		if out != "" {
			next = out + "+" + w
		}
		if len(next) > MaxLabelValue {
			break
		}
		out = next
	}
	return out
}

// MaxCanWords is how many capability words one can= term may name: each becomes four
// patterns (see ExpandCaps) and a selector term holds at most MaxAlternatives (8), the limit
// older nodes enforce too.
const MaxCanWords = MaxAlternatives / 4

var capWordRe = regexp.MustCompile(`^[a-z0-9._/-]+$`)

// ExpandCaps rewrites the sender-side term can=run of a selector target into a whole-word
// match on the caps label, which is a sorted '+'-joined list: "@repo=x,can=run" becomes
// "@repo=x,caps=run|run+*|*+run|*+run+*" (the word alone, first, last or in the middle), so
// "dry-run", "cannot-run" or "runaway" do not count as "run". can!=run is the same with !=, and
// excludes exactly the sessions that advertise the word. can=* is any session with any caps.
// Like @here it is resolved when the message is sent, so nodes only ever see ordinary labels
// and a form every node reads. A target without a can term comes back unchanged;
// errors quote the term as the sender typed it.
func ExpandCaps(to string) (string, error) {
	return expandWords(to, "can", CapsLabel, "capabilities", "can=run")
}

// RoomsLabel is the label a session lists the rooms it is in: room names joined with '+' (see
// CleanCaps, which cleans it the same way). `aivoicebus room join` keeps it for the session.
const RoomsLabel = "rooms"

// ExpandRooms is ExpandCaps for rooms: the term room=release-0.5 of a selector becomes a
// whole-word match on the rooms label, so a message to @room=release-0.5 reaches every session in it.
func ExpandRooms(to string) (string, error) {
	return expandWords(to, "room", RoomsLabel, "rooms", "room=release-0.5")
}

// expandWords rewrites the term key=word of a selector into a whole-word match on label, a
// sorted '+'-joined list (see ExpandCaps for the four patterns per word).
func expandWords(to, key, label, what, example string) (string, error) {
	body, ok := strings.CutPrefix(to, "@")
	if !ok {
		return to, nil
	}
	terms := strings.Split(body, ",")
	changed := false
	for i, raw := range terms {
		t := strings.TrimSpace(raw)
		k, val, op := "", "", "="
		if a, b, found := strings.Cut(t, "!="); found {
			k, val, op = a, b, "!="
		} else if a, b, found := strings.Cut(t, "="); found {
			k, val = a, b
		}
		if strings.ToLower(strings.TrimSpace(k)) != key {
			continue
		}
		changed = true
		if val == "*" {
			terms[i] = label + op + "*"
			continue
		}
		words := strings.Split(strings.ToLower(val), "|")
		if len(words) > MaxCanWords {
			return "", fmt.Errorf("selector term %q names more than %d %s (each one takes four of the %d alternatives a selector term may have)", t, MaxCanWords, what, MaxAlternatives)
		}
		var pats []string
		for _, w := range words {
			if !capWordRe.MatchString(w) {
				return "", fmt.Errorf("selector term %q needs whole words (lowercase letters, digits, . _ / -; no spaces, + or *), such as %s; %s=* is any", t, example, key)
			}
			pats = append(pats, w, w+"+*", "*+"+w, "*+"+w+"+*")
		}
		terms[i] = label + op + strings.Join(pats, "|")
	}
	if !changed {
		return to, nil
	}
	return "@" + strings.Join(terms, ","), nil
}

// glob matches s against pattern, where '*' matches any run of characters
// (including '/', so branch=feature/* works) and everything else matches itself.
func glob(pattern, s string) bool {
	p, i, star, mark := 0, 0, -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			mark++
			p, i = star+1, mark
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
