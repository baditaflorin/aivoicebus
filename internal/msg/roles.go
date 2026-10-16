package msg

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Role aliases: "@build" means "the session that is the build session right now". A session
// claims a role with the label role=build (several: role=build+review), which nodes already carry
// in the roster, so nothing about roles is replicated or stored with messages. The sender looks
// the role up in the roster when it sends and addresses the one live holder by its identity.
//
// Labels are self-declared, so a role claim is a hint, never proof: any holder of the shared token
// can claim any role, and one that is idle for RoleLive loses it to whoever claims it next. So:
//   - a role the sender pinned (~/.config/aivoicebus/roles) resolves, within the pin, to its one
//     live holder; claims from sessions outside the pin are ignored and reported;
//   - a role nobody pinned resolves only when exactly one identity has ever claimed it in the
//     roster (live or not) and the sender says it trusts that (RoleOpts.Trust), and never for an
//     urgent message or one that carries a file or log link;
//   - two claimants of an unpinned role, two live holders of a pinned one, or a holder not heard
//     from within RoleLive is an error that names them, never a guess;
//   - whoever it resolved to is always shown, with whether the role was pinned.
const (
	RoleLabel = "role"
	// RoleLive is how recently a session must have read from a node to be the current holder.
	RoleLive = 2 * time.Hour
	// roleSkew is how far ahead of this clock a sighting may be before it is not believed.
	roleSkew = 5 * time.Minute
	// MaxRoles is how many roles one session may claim.
	MaxRoles = 4
)

// IsRoleAlias reports whether a target is a role alias ("@build") and returns the role. The
// other @ forms are selectors (@k=v), @here and @+group, none of which is a valid role name.
func IsRoleAlias(to string) (string, bool) {
	name, ok := strings.CutPrefix(to, "@")
	if !ok || !ValidRoleName(name) {
		return "", false
	}
	return name, true
}

// RolesOf are the roles a session claims: the valid names in its role label, at most MaxRoles.
func RolesOf(l Labels) []string {
	var out []string
	for _, r := range strings.Split(l[RoleLabel], "+") {
		if ValidRoleName(r) && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	if len(out) > MaxRoles {
		out = out[:MaxRoles]
	}
	return out
}

// ValidRolePin reports why pattern cannot pin a role, or nil. A pin is "host/agent" in lower case
// with '*' wildcards that stay inside their part (a '*' never crosses the '/'), and its host part
// must name something: "*", "*/claude.*" and a pattern with no host would let anyone in.
func ValidRolePin(p string) error {
	host, agent, ok := strings.Cut(p, "/")
	switch {
	case p == "" || len(p) > MaxIdentity || hasControl(p) || invisibleProblem(p) != "":
		return errors.New("a pin is a short identity pattern without control or invisible characters")
	case !ok || host == "" || agent == "" || strings.Contains(agent, "/"):
		return fmt.Errorf("pin %q must look like host/agent (e.g. build-*/claude.*): it needs a host part", SafeName(p))
	case strings.Trim(host, "*") == "":
		return fmt.Errorf("pin %q does not name a host: a pin that lets every machine in pins nothing", SafeName(p))
	case p != strings.ToLower(p):
		return fmt.Errorf("pin %q must be lower case", SafeName(p))
	}
	return nil
}

// ParseRolePins reads the roles file: "role=pattern[,pattern...]" per line, "#" starts a comment.
// A line that is not a valid role, and a pattern that is not a valid pin (ValidRolePin), is
// skipped, so one typo does not hide the other roles.
func ParseRolePins(text string) map[string][]string {
	pins := map[string][]string{}
	for _, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		name, pats, ok := strings.Cut(strings.TrimSpace(line), "=")
		name = strings.ToLower(strings.TrimSpace(name))
		if !ok || !ValidRoleName(name) {
			continue
		}
		for _, p := range strings.FieldsFunc(strings.ToLower(pats), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if ValidRolePin(p) == nil && !slices.Contains(pins[name], p) {
				pins[name] = append(pins[name], p)
			}
		}
	}
	return pins
}

// pinMatches reports whether the identity me fits the pin: host and agent each match their part,
// so a '*' never reaches across the '/'.
func pinMatches(pin, me string) bool {
	ph, pa, _ := strings.Cut(pin, "/")
	mh, ma, ok := strings.Cut(strings.ToLower(me), "/")
	return ok && !strings.Contains(ma, "/") && glob(ph, mh) && glob(pa, ma)
}

// RoleHolder is a session claiming a role.
type RoleHolder struct {
	Me   string
	Seen time.Time
}

// RoleOpts says what the sender accepts for this send.
type RoleOpts struct {
	Trust  bool // an unpinned role with a single claimant may be used (--trust-role)
	Urgent bool // the message is urgent: an unpinned role is never used for it
	Attach bool // it carries a file or a log link: likewise
}

// RoleResolution is what a role alias came to.
type RoleResolution struct {
	Role   string
	Pins   []string     // the patterns the role is pinned to (none when it is not)
	Live   []RoleHolder // holders heard from within RoleLive, newest first
	Stale  []RoleHolder // holders heard from longer ago
	Out    []string     // sessions claiming the role that the pin ignored
	Target string       // the identity to send to; "" when it did not resolve
}

// Pinned reports whether the sender pinned the role.
func (r RoleResolution) Pinned() bool { return len(r.Pins) > 0 }

// Trust is how a resolved role is labelled to the sender: it matches the sender's pin (which proves only
// that the identity fits the pattern: anyone with the token can use any identity) or is unverified.
func (r RoleResolution) Trust() string {
	if r.Pinned() {
		return "matches your pin pattern; identities are self-declared"
	}
	return "unverified"
}

// Line is the one line printed for a resolved role: resolved @build -> host/agent.sess (pinned).
func (r RoleResolution) Line() string {
	return fmt.Sprintf("resolved @%s -> %s (%s)", r.Role, SafeName(r.Target), r.Trust())
}

// PinHint is the command that pins role to the machine and tool of identity me.
func PinHint(role, me string) string {
	host, agent, _ := strings.Cut(strings.ToLower(me), "/")
	tool, _, _ := strings.Cut(agent, ".")
	return fmt.Sprintf("aivoicebus role pin %s '%s/%s.*'", role, SafeName(host), SafeName(tool))
}

// ResolveRole finds the session holding role in the roster. me is the asker, who never resolves
// to itself. The returned resolution is filled in as far as it got even on an error, so `who
// @role` can show the holders; the error says what to do. Entries whose identity could pass for
// another (SenderProblem, NameProblem) are never holders.
func ResolveRole(role string, roster []RosterEntry, pins map[string][]string, me string, now time.Time, o RoleOpts) (RoleResolution, error) {
	res := RoleResolution{Role: role, Pins: pins[role]}
	if !ValidRoleName(role) {
		return res, fmt.Errorf("%q is not a role name", role)
	}
	self := false
	for _, e := range roster {
		if !ValidIdentity(e.Me) || SenderProblem(e.Me) != "" || NameProblem(e.Me) != "" || !slices.Contains(RolesOf(e.Labels), role) {
			continue
		}
		if e.Me == me {
			self = true
			continue
		}
		if res.Pinned() && !slices.ContainsFunc(res.Pins, func(p string) bool { return pinMatches(p, e.Me) }) {
			res.Out = append(res.Out, e.Me)
			continue
		}
		h := RoleHolder{e.Me, e.Seen}
		if age := now.Sub(e.Seen); age > RoleLive || age < -roleSkew {
			res.Stale = append(res.Stale, h)
		} else {
			res.Live = append(res.Live, h)
		}
	}
	newestFirst := func(a, b RoleHolder) int { return b.Seen.Compare(a.Seen) }
	slices.SortStableFunc(res.Live, newestFirst)
	slices.SortStableFunc(res.Stale, newestFirst)
	all := append(slices.Clone(res.Live), res.Stale...)

	how := "@" + role
	if res.Pinned() {
		how += " (pinned to " + pinList(res.Pins) + ")"
	}
	stale := func() error {
		return fmt.Errorf("%s has no live holder: %s (live means heard from in the last %s); send to it by identity if it is the one you mean, or wait for it to start",
			how, holderList(res.Stale, now), SpanWords(RoleLive))
	}
	switch {
	case res.Pinned() && len(res.Live) == 1:
		res.Target = res.Live[0].Me // as the node holds it: lower-casing an identity with capitals would reach nobody
		return res, nil
	case res.Pinned() && len(res.Live) > 1:
		return res, fmt.Errorf("%s is claimed by %d live sessions within the pin: %s; send to one of them by identity", how, len(res.Live), holderList(res.Live, now))
	case res.Pinned() && len(res.Stale) > 0:
		return res, stale()
	case res.Pinned() && len(res.Out) > 0:
		return res, fmt.Errorf("%s has no holder you pinned: ignored claim(s) from %s", how, nameList(res.Out))
	case !res.Pinned() && len(all) > 1:
		return res, fmt.Errorf("%s is claimed by %d sessions: %s; an unpinned role is never guessed between claimants. Pin it to the one you trust: %s",
			how, len(all), holderList(all, now), PinHint(role, all[0].Me))
	case !res.Pinned() && len(all) == 1:
		hint := PinHint(role, all[0].Me)
		switch {
		case len(res.Stale) == 1:
			return res, fmt.Errorf("%s is claimed only by %s, not heard from in the last %s (live means that); an idle holder is how a role is taken over. Pin it to the session you trust: %s",
				how, holderList(res.Stale, now), SpanWords(RoleLive), hint)
		case o.Urgent || o.Attach:
			return res, fmt.Errorf("%s is not pinned, and an unpinned role is never used for an urgent message or a file or log link, even with --trust-role (anyone with the token can claim a role). Pin it: %s", how, hint)
		case !o.Trust:
			return res, fmt.Errorf("%s is claimed only by %s, but anyone with the token can claim a role. Pin it to the session you trust: %s (or add --trust-role to use this claim once)", how, holderList(res.Live, now), hint)
		}
		res.Target = res.Live[0].Me // as the node holds it: lower-casing an identity with capitals would reach nobody
		return res, nil
	case self:
		return res, fmt.Errorf("only this session claims %s", how)
	}
	return res, fmt.Errorf("no session claims %s (a session claims a role with the label role=%s, e.g. AIVOICEBUS_ROLE=%s)", how, role, role)
}

func pinList(pins []string) string {
	out := make([]string, len(pins))
	for i, p := range pins {
		out[i] = SafeName(p)
	}
	return strings.Join(out, ", ")
}

func holderList(hs []RoleHolder, now time.Time) string {
	const shown = 5
	var parts []string
	for _, h := range hs[:min(len(hs), shown)] {
		parts = append(parts, fmt.Sprintf("%s (seen %s ago)", SafeName(h.Me), SpanWords(now.Sub(h.Seen))))
	}
	if more := len(hs) - shown; more > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", more))
	}
	return strings.Join(parts, ", ")
}

func nameList(names []string) string {
	const shown = 3
	var parts []string
	for _, n := range names[:min(len(names), shown)] {
		parts = append(parts, SafeName(n))
	}
	if more := len(names) - shown; more > 0 {
		parts = append(parts, fmt.Sprintf("+%d more", more))
	}
	return strings.Join(parts, ", ")
}

// SpanWords writes a duration for a person: "45s", "12m", "3h".
func SpanWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d/time.Second), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%dh", int(d/time.Hour))
}

// Describe is the resolved role with how it was decided, shown with the send.
func (r RoleResolution) Describe() string {
	if r.Pinned() {
		return fmt.Sprintf("@%s resolved to %s (pinned to %s)", r.Role, SafeName(r.Target), pinList(r.Pins))
	}
	return fmt.Sprintf("@%s resolved to %s: its own role label, not verified, used because you trusted it", r.Role, SafeName(r.Target))
}
