package msg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Names: which sender and target names are unusable, in one place. Identities are self-declared, so
// the rules are about what a reader can be fooled by, never about who owns a name. The send
// refusal (the node's /v1/send and the CLI before it), the checks on names people choose (roles,
// persistent agent names) and the display of peers' names all come through here.
//
// There is no Unicode normalisation in the standard library, so everything rests on fold: one
// function saying what a reader sees a character as. The refusal does not bar non-ASCII names
// (real hostnames have them: сервер, école); it bars what is invisible, what claims the machine
// the nodes speak as, and what passes for a reserved word or a tool name once folded.

// SystemHost is the machine part of the senders only a node speaks as (NoticeFrom). Notices are
// never stored, so a message in the log whose sender claims this machine is forged: nodes refuse to
// accept one (/v1/send) and never deliver one that arrived by replication (see IsSystemSender).
const SystemHost = "aivoicebus"

// ToolNames are the agent tools whose names a session's agent part starts with (claude.a1b2). A
// name that only passes for one is refused. identity's tool table must be inside this list (tested).
var ToolNames = []string{"claude", "codex", "gemini", "grok", "cursor-agent", "opencode", "aider", "goose"}

// invisible is a character a reader cannot see: a format or bidi control, a separator, a space other
// than the plain one, and the blank letters that are not format controls (braille blank, Hangul
// fillers, Khmer inherent vowels, Mongolian variation selectors, variation selectors, tag characters).
func invisible(r rune) bool {
	switch {
	case unicode.In(r, unicode.Cf, unicode.Zs, unicode.Zl, unicode.Zp):
		return true
	case r == 0x2800 || r == 0x3164 || r == 0x115f || r == 0x1160 || r == 0xffa0 || r == 0x17b4 || r == 0x17b5 || r == 0x034f:
		return true
	case r >= 0x180b && r <= 0x180f, r >= 0xfe00 && r <= 0xfe0f, r >= 0xe0000 && r <= 0xe007f, r >= 0xe0100 && r <= 0xe01ef:
		return true
	}
	return false
}

// HasInvisible reports whether s holds a character a reader cannot see (the plain space is fine).
// A sender's identity with one can pass for another's; /v1/send refuses it (replicated records are
// taken as they are).
func HasInvisible(s string) bool {
	for _, r := range s {
		if invisible(r) && r != ' ' {
			return true
		}
	}
	return false
}

// foldTable is what characters that pass for ASCII ones fold to; see lookalike for the ranges.
var foldTable = func() map[rune]string {
	m := map[rune]string{}
	each := func(to, from string) {
		for _, r := range from {
			m[r] = to
		}
	}
	pairs := func(s string) { // "ᴀa ʙb": the character, then what it passes for
		for _, p := range strings.Fields(s) {
			r := []rune(p)
			m[r[0]] = strings.ToLower(string(r[1:]))
		}
	}
	each("-", "֊־᐀᠆‐‑‒–—―⁻₋−⸺⸻⹀⁃˗﹘﹣－ー")
	each("/", "⁄∕⧸╱᜵⟋／")
	each(".", "․。﹒܁܂۔։ꓸ")
	pairs("ᴀa ʙb ᴄc ᴅd ᴇe ꜰf ɢg ʜh ɪi ᴊj ᴋk ʟl ᴍm ɴn ᴏo ᴘp ʀr ꜱs ᴛt ᴜu ᴠv ᴡw ʏy ᴢz")                      // Latin small capitals
	pairs("ſs ƒf ıi ȷj ɑa ɡg ɩi ⱼj ǀl")                                                                   // long s, f with hook, dotless, script forms
	pairs("ﬀff ﬁfi ﬂfl ﬃffi ﬄffl ﬅst ﬆst ĳij ǉlj ǌnj ǆdz ǳdz")                                            // ligatures
	pairs("аa сc еe оo рp хx уy іi јj ѕs һh ԁd ԛq ԝw ӏl ѵv ѡw кk мm нh тt вb")                            // Cyrillic
	pairs("οo νv ιi ρp υu κk αa εe χx")                                                                   // Greek
	pairs("սu օo ոn հh ցg զq")                                                                            // Armenian
	pairs("ᎪA ᏴB ᏟC ᎠD ᎬE ᏀG ᎻH ᎥI ᎫJ ᏦK ᏞL ᎷM ᏢP ᏓS ᏚS ᎢT ᏙV ᏔW ᏃZ")                                     // Cherokee
	pairs("ℂc ℊg ℋh ℌh ℍh ℎh ℐi ℑi ℒl ℓl ℕn ℙp ℚq ℛr ℜr ℝr ℤz ℨz ℬb ℭc ℯe ℰe ℱf ℳm ℴo ℹi ⅅd ⅆd ⅇe ⅈi ⅉj") // letterlike
	pairs("ᵃa ᵇb ᶜc ᵈd ᵉe ᶠf ᵍg ʰh ⁱi ʲj ᵏk ˡl ᵐm ⁿn ᵒo ᵖp ʳr ˢs ᵗt ᵘu ᵛv ʷw ˣx ʸy ᶻz")                   // superscript letters
	pairs("ₐa ₑe ₕh ᵢi ₖk ₗl ₘm ₙn ₒo ₚp ᵣr ₛs ₜt ᵤu ᵥv ₓx")                                              // subscript letters
	pairs("ⅰi ⅱii ⅲiii ⅳiv ⅴv ⅵvi ⅶvii ⅷviii ⅸix ⅹx ⅺxi ⅻxii ⅼl ⅽc ⅾd ⅿm")                                // roman numerals
	for i, d := range "0123456789" {
		m[0x2070+rune(i)], m[0x2080+rune(i)] = string(d), string(d) // superscript and subscript digits
	}
	// small form variants (U+FE50..U+FE6B): ﹒ ﹣ ﹨ ...
	for from, to := range map[rune]string{0xfe50: ",", 0xfe52: ".", 0xfe54: ";", 0xfe55: ":", 0xfe56: "?", 0xfe57: "!", 0xfe59: "(", 0xfe5a: ")",
		0xfe5b: "{", 0xfe5c: "}", 0xfe5f: "#", 0xfe60: "&", 0xfe61: "*", 0xfe62: "+", 0xfe64: "<", 0xfe65: ">", 0xfe66: "=", 0xfe68: "\\", 0xfe69: "$", 0xfe6a: "%", 0xfe6b: "@"} {
		m[from] = to
	}
	return m
}()

// lookalike is what r passes for in ASCII, for the characters in the ranges that are too long to
// list: fullwidth forms, mathematical alphanumerics, enclosed alphanumerics.
func lookalike(r rune) (string, bool) {
	switch {
	case r >= 0xff01 && r <= 0xff5e: // fullwidth ASCII
		return string(unicode.ToLower(r - 0xfee0)), true
	case r >= 0x1d400 && r <= 0x1d6a3: // mathematical letters, A-Z then a-z per style
		return string('a' + (r-0x1d400)%52%26), true
	case r == 0x1d6a4:
		return "i", true
	case r == 0x1d6a5:
		return "j", true
	case r >= 0x1d7ce && r <= 0x1d7ff: // mathematical digits
		return string('0' + (r-0x1d7ce)%10), true
	case r == 0x24ea || r == 0x24ff:
		return "0", true
	}
	for _, g := range [...]struct{ lo, hi, first rune }{ // ① ⑴ ⒈ and the negative and double circled ones
		{0x2460, 0x2473, 1}, {0x2474, 0x2487, 1}, {0x2488, 0x249b, 1}, {0x24eb, 0x24f4, 11}, {0x24f5, 0x24fe, 1},
		{0x2776, 0x277f, 1}, {0x2780, 0x2789, 1}, {0x278a, 0x2793, 1},
	} {
		if r >= g.lo && r <= g.hi {
			return strconv.Itoa(int(r - g.lo + g.first)), true
		}
	}
	for _, g := range [...]struct{ lo, hi rune }{
		{0x249c, 0x24b5}, {0x24b6, 0x24cf}, {0x24d0, 0x24e9}, // ⒜ Ⓐ ⓐ
		{0x1f110, 0x1f129}, {0x1f130, 0x1f149}, {0x1f150, 0x1f169}, {0x1f170, 0x1f189}, {0x1f1e6, 0x1f1ff}, // parenthesised, squared, negative, regional
	} {
		if r >= g.lo && r <= g.hi {
			return string('a' + (r - g.lo)), true
		}
	}
	return "", false
}

// fold is what a reader sees r as: "" when it is not seen at all (invisible, a combining mark), the
// ASCII it passes for (a lookalike), the character itself, or its lower case. One function, so
// Skeleton, the refusals and the lint cannot disagree.
func fold(r rune) string {
	switch {
	case r == ' ':
		return " "
	case r < 0x80:
		return string(unicode.ToLower(r))
	case invisible(r) || unicode.In(r, unicode.Mn, unicode.Me):
		return ""
	}
	if s, ok := foldTable[r]; ok {
		return s
	}
	if s, ok := lookalike(r); ok {
		return s
	}
	l := unicode.ToLower(r)
	if s, ok := foldTable[l]; ok {
		return s
	}
	return string(l)
}

// passesForAscii reports whether non-ASCII r passes for something else in ASCII: its fold is
// neither itself nor just its own lower case. (K and İ lower-case to ASCII, so they do.)
func passesForAscii(r rune) bool {
	if r < 0x80 || invisible(r) || unicode.In(r, unicode.Mn, unicode.Me) {
		return false
	}
	f, l := fold(r), unicode.ToLower(r)
	return f != string(r) && !(l >= 0x80 && f == string(l))
}

// Skeleton is s as a reader would see it. Two names with the same skeleton can be mistaken for one
// another, so comparisons that guard a name are made on skeletons.
func Skeleton(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteString(unaccent(fold(r)))
	}
	return b.String()
}

// accentTable is the base letters of the precomposed Latin-1 and Latin Extended-A letters (and the
// ligatures and ƽ that pass for ASCII ones). Only the skeleton uses it: "cláude" collides with
// "claude" (a tool name) and "ímac" with "imac", but fold, and so the lint and the display, leave
// accented letters alone, so a real hostname like "café" or "école" is never flagged for its accent.
var accentTable = func() map[rune]string {
	m := map[rune]string{}
	for _, p := range strings.Fields("àa áa âa ãa äa åa āa ăa ąa çc ćc ĉc ċc čc ďd đd èe ée êe ëe ēe ĕe ėe ęe ěe ĝg ğg ġg ģg ĥh ħh " +
		"ìi íi îi ïi ĩi īi ĭi įi ĵj ķk ĺl ļl ľl ŀl łl ñn ńn ņn ňn ŉn òo óo ôo õo öo øo ōo ŏo őo ŕr ŗr řr śs ŝs şs šs ţt ťt ŧt " +
		"ùu úu ûu üu ũu ūu ŭu ůu űu ųu ŵw ýy ÿy ŷy źz żz žz ðd ƽs ßss æae œoe") {
		r := []rune(p)
		m[r[0]] = string(r[1:])
	}
	return m
}()

// unaccent maps a one-character fold that is an accented Latin letter to its base letters.
func unaccent(f string) string {
	if r := []rune(f); len(r) == 1 && r[0] >= 0x80 {
		if b, ok := accentTable[r[0]]; ok {
			return b
		}
	}
	return f
}

// fullwidth reports whether r is a fullwidth ASCII form (ｉｍａｃ): shown marked even in a name that
// has no ASCII letter at all, since nothing but a disguise has a reason to be written that way here.
func fullwidth(r rune) bool { return r >= 0xff01 && r <= 0xff5e }

// projection is Skeleton with everything that is still not ASCII removed: what is left of a name
// once the foreign characters are taken out, so one stray character cannot hide a word.
func projection(s string) string {
	var b strings.Builder
	for _, r := range s {
		for _, f := range fold(r) { // fold, not Skeleton: an accented letter is taken out, not made ASCII
			if f < 0x80 {
				b.WriteRune(f)
			}
		}
	}
	return b.String()
}

// IsSystemSender reports whether from claims to be the mesh itself: its machine part, as a
// skeleton or as a projection, is SystemHost. So "AiVoiceBus/retraction", "aivoice‑bus/x" (a
// Unicode hyphen), "aivoicebusㅤ/x" (a blank letter), "ꜰleet-msg/x" (a small capital) and
// "aivoicebus/anything" all claim it.
func IsSystemSender(from string) bool {
	sk := unspaced(Skeleton(from))
	return HostOf(sk) == SystemHost || HostOf(unspaced(projection(from))) == SystemHost
}

// unspaced is s without any space or control character (a plain space, a tab, a no-break or
// ideographic space): fold keeps the plain space and ASCII controls, but "aivoice bus/x" reads as
// the system sender to anyone, so the claim is judged with them gone.
func unspaced(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

var reservedWords = func() map[string]bool {
	m := map[string]bool{}
	for w := range ReservedAgents {
		m[w] = true
	}
	for _, w := range ToolNames {
		m[w] = true
	}
	return m
}()

// collision says which reserved word or tool name a name passes for without being it, "" if none:
// its machine part or agent tool, as a skeleton or a projection, is one, though the name itself is
// spelled differently. A real hostname in another script is no collision.
func collision(name string) string {
	plain := strings.ToLower(name)
	plainHost, plainAgent, _ := strings.Cut(plain, "/")
	plainTool, _, _ := strings.Cut(plainAgent, ".")
	for _, v := range [...]string{Skeleton(name), projection(name)} {
		host, agent, _ := strings.Cut(v, "/")
		tool, _, _ := strings.Cut(agent, ".")
		switch {
		case reservedWords[host] && host != plainHost:
			return host
		case reservedWords[tool] && tool != plainTool:
			return tool
		}
	}
	return ""
}

func hasASCIIAlnum(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// NameProblem is the lint: why a name could be mistaken for another, "" when it cannot. It finds
// an invisible character and, in a name that also has ASCII letters (a mix of scripts), a combining
// mark or a character that passes for an ASCII one. A name wholly in another script is not
// reported: it is a real name, only a mixed one is a disguise. The host and the agent part (either
// side of the '/') are judged apart.
func NameProblem(s string) string {
	for _, part := range strings.Split(s, "/") { // the host and the agent are judged apart: сервер/claude.a1b2 is two plain names
		mixed := hasASCIIAlnum(part)
		for _, r := range part {
			switch {
			case invisible(r) && r != ' ':
				return fmt.Sprintf("holds an invisible character (U+%04X: a format or bidi control, a blank letter or a separator)", r)
			case fullwidth(r):
				return fmt.Sprintf("holds U+%04X, a fullwidth form that passes for %q", r, fold(r))
			case !mixed:
			case unicode.In(r, unicode.Mn, unicode.Me):
				return fmt.Sprintf("holds a combining mark (U+%04X) among ASCII letters", r)
			case passesForAscii(r):
				return fmt.Sprintf("holds U+%04X, which passes for %q among ASCII letters", r, fold(r))
			}
		}
	}
	return ""
}

// invisibleProblem is the refusal for a name that holds a character no one can see.
func invisibleProblem(s string) string {
	for _, r := range s {
		if invisible(r) && r != ' ' {
			return fmt.Sprintf("holds an invisible character (U+%04X: a format or bidi control, a blank letter or a separator)", r)
		}
	}
	return ""
}

// SenderProblem is why a message may not be sent as from, "" when it may: an invisible character,
// a claim on the machine the nodes speak as, or a name that passes for a reserved word or a tool
// (a Cyrillic "clаude"). It reads as a predicate of the name. A name that is only not ASCII (a
// real hostname) is never refused.
func SenderProblem(from string) string {
	if p := invisibleProblem(from); p != "" {
		return p
	}
	if IsSystemSender(from) {
		return "claims " + SystemHost + ", the machine the nodes speak as (retraction notices); no one sends as it"
	}
	if w := collision(from); w != "" {
		return fmt.Sprintf("passes for %q without being it", w)
	}
	return ""
}

// TargetProblem is SenderProblem for the target of a message: the machine the nodes speak as is not
// special there, since a message to it reaches no one and harms no one.
func TargetProblem(to string) string {
	if p := invisibleProblem(to); p != "" {
		return p
	}
	if w := collision(to); w != "" {
		return fmt.Sprintf("passes for %q without being it", w)
	}
	return ""
}

// SafeName is an identity, role or label from a peer made safe to show: control characters become
// '?', and an invisible character, or among ASCII letters a combining mark or a lookalike, is
// written as <U+XXXX>, so a name that only looks like another one reads differently. Plain names,
// and names wholly in another script, come back unchanged.
func SafeName(s string) string {
	spoof := IsSystemSender(s) && strings.ToLower(HostOf(s)) != SystemHost // "aivoice bus/x", not a node's own "aivoicebus/x"
	var b strings.Builder
	for i, part := range strings.Split(s, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		mixed := hasASCIIAlnum(part)
		for _, r := range part {
			switch {
			case r == ' ' && spoof: // a space inside a name that reads as the system sender is a disguise: show it
				b.WriteString("<U+0020>")
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
				b.WriteByte('?')
			case (invisible(r) && r != ' ') || fullwidth(r) || (mixed && (unicode.In(r, unicode.Mn, unicode.Me) || passesForAscii(r))):
				fmt.Fprintf(&b, "<U+%04X>", r)
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// ReservedAgents are agent names no one may choose for a persistent identity or a role: the
// mesh's own, and words that are target syntax.
// The join probe identities are reserved here (not at the identity layer, which the probe itself
// uses), so no registered name or role claim can pass for one: a probe is left out of rosters.
var ReservedAgents = map[string]bool{"aivoicebus": true, "system": true, "all": true, "here": true, "mesh": true, "agent": true, "mcp": true,
	ProbeSenderAgent: true, ProbeReaderAgent: true}

// MinPersistentSuffix is the shortest suffix of a chosen agent name (codex.build1). Every
// automatic session suffix is at most four characters (the Claude hook's four letters, a
// four-character hash, mcp.<4>), so a chosen name of five or more can never equal one.
const MinPersistentSuffix = 5

var persistentRe = regexp.MustCompile(`^([a-z][a-z0-9_-]{0,23})\.([a-z0-9][a-z0-9_-]{4,23})$`)

// ValidPersistentName reports why name cannot be a long-lived agent name ("tool.label", e.g.
// codex.build1), or nil. It must be ASCII lower case, its label at least MinPersistentSuffix long,
// and neither part reserved.
func ValidPersistentName(name string) error {
	m := persistentRe.FindStringSubmatch(name)
	switch {
	case m == nil:
		return fmt.Errorf("%q must look like tool.label, e.g. codex.build1: lower-case letters, digits, - and _, the label at least %d characters", name, MinPersistentSuffix)
	case ReservedAgents[m[1]] || ReservedAgents[m[2]]:
		return fmt.Errorf("%q uses a reserved name", name)
	}
	return nil
}

// labelKeys are the label keys sessions declare themselves; "@repo" is a selector missing its
// value, not a role, so a typo gets the selector's explanation.
var labelKeys = map[string]bool{"machine": true, "tool": true, "host": true, "chip": true, "product": true, "repo": true, "origin": true, "branch": true, RoleLabel: true}

// ValidRoleName reports whether name can be a role (@build): a label key shape that is neither
// target syntax nor the name of a label.
func ValidRoleName(name string) bool {
	return ValidKey(name) && !ReservedAgents[name] && !labelKeys[name]
}
