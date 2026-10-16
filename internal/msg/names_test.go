package msg

import (
	"strings"
	"testing"
	"unicode"
)

// styled writes s with each lower-case letter moved by offset from 'a' to base (a styled alphabet).
func styled(s string, base rune) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			r = base + (r - 'a')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// every class of lookalike the machine part of aivoicebus can be written in
func systemSpellings() map[string]string {
	m := map[string]string{
		"ASCII upper case":           "AiVoiceBus",
		"Latin small capitals":       "ᴀɪᴠᴏɪᴄᴇʙᴜꜱ",
		"small capitals, mostly":     "ᴀivoicebᴜꜱ",
		"mathematical bold":          styled("aivoicebus", 0x1d41a),
		"mathematical double-struck": styled("aivoicebus", 0x1d552),
		"mathematical monospace":     styled("aivoicebus", 0x1d68a),
		"circled":                    styled("aivoicebus", 0x24d0),
		"parenthesised":              styled("aivoicebus", 0x249c),
		"fullwidth":                  styled("aivoicebus", 0xff41),
		"squared":                    styled("aivoicebus", 0x1f130),
		"Cyrillic":                   "аіvоісеbus",
		"Cyrillic, other letters":    "aivoicebuѕ",
		"Greek":                      "aivοicebus",
		"Greek nu":                   "aiνoicebus",
		"dotless i":                  "aıvoıcebus",
	}
	for _, r := range []rune{0x2800, 0x3164, 0x115f, 0x1160, 0xffa0, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180e, 0x180f, 0xe0001, 0xe0020, 0xe007f, 0xfe0f, 0x034f, 0x200b, 0x2060, 0x00a0, 0x202e} {
		m["invisible "+string(r)+" at the end"] = "aivoicebus" + string(r)
		m["invisible "+string(r)+" inside"] = "aivo" + string(r) + "icebus"
	}
	return m
}

func TestEveryLookalikeOfTheNodesMachineIsASystemSender(t *testing.T) {
	for name, host := range systemSpellings() {
		for _, rest := range []string{"/retraction", "/claude.a1b2"} {
			if !IsSystemSender(host + rest) {
				t.Errorf("%s: %+q passes for the nodes' machine", name, host+rest)
			}
			if SenderProblem(host+rest) == "" {
				t.Errorf("%s: %+q would be sent", name, host+rest)
			}
		}
	}
	// the slash itself can be a lookalike: there is then no '/' at all
	for _, sl := range []rune{0x2044, 0x2215, 0xff0f, 0x29f8, 0x2571} {
		if from := "aivoicebus" + string(sl) + "x"; !IsSystemSender(from) {
			t.Errorf("%+q", from)
		}
	}
	// the exact example from the review: a Hangul filler before the slash
	if !IsSystemSender("aivoicebusㅤ/retraction") {
		t.Error("aivoicebusㅤ/retraction")
	}
	for from, want := range map[string]bool{
		NoticeFrom:               true,
		"aivoicebus/anything":    true,
		"aivoicebuss/x":          false,
		"aivoice_bus/x":          false,
		"aivoicebus-bot/x":       false,
		"laptop/aivoicebus":      false, // only the machine part is reserved
		"laptop/aivoice‑bus":     false,
		"сервер/claude.a1b2":     false, // real hostnames
		"école/claude.a1b2":      false,
		"école/claude.a1b2":     false,
		"中文/claude.a1b2":         false,
		"aivoicebusé/claude.a1b": true, // once the foreign character is taken out, it is the nodes' machine
	} {
		if got := IsSystemSender(from); got != want {
			t.Errorf("%+q: %v", from, got)
		}
	}
}

func TestRealHostnamesInOtherScriptsAreNeverRefused(t *testing.T) {
	for _, name := range []string{
		"сервер/claude.a1b2", "école/codex.q1w2", "école/codex.q1w2", "中文/claude.a1b2", "laptop-a/claude.a1b2",
		"Laptop/Claude.A1B2", "Laptop Pro/claude", "münchen-1/gemini.zz01", "köln/claude.1234",
	} {
		if why := SenderProblem(name); why != "" {
			t.Errorf("sender %+q: %s", name, why)
		}
		if why := TargetProblem(name); why != "" {
			t.Errorf("target %+q: %s", name, why)
		}
	}
	// the lint does not apply to a name wholly in one script
	if why := NameProblem("сервер"); why != "" {
		t.Errorf("a wholly Cyrillic name is a real one: %s", why)
	}
	if SafeName("сервер/claude.a1b2") != "сервер/claude.a1b2" || SafeName("école/claude.a1b2") != "école/claude.a1b2" {
		t.Error("a real name is shown as it is")
	}
}

func TestANameThatPassesForAToolOrAReservedWordIsRefused(t *testing.T) {
	for _, name := range []string{
		"laptop/clаude.a1b2", "laptop/c⠀laude.a1b2", "laptop/ᴄlaude.a1b2", "laptop/codexㅤ.q1w2", "laptop/ɡemini.zz",
		"laptop/sуstem", "laptop∕claude.a1", "laptop/ⓒlaude.a1b2", "laptop/cursor‑agent.ab12", "laptop/аll",
	} {
		if SenderProblem(name) == "" || TargetProblem(name) == "" {
			t.Errorf("%+q", name)
		}
	}
	for _, name := range []string{"laptop/claude.a1b2", "all", "claude", "laptop/claude", "laptop", "claude/claude.a1b2"} { // spelled as they are
		if why := TargetProblem(name); why != "" {
			t.Errorf("%q: %s", name, why)
		}
	}
	if why := SenderProblem("laptop/claude.a1b2"); why != "" {
		t.Error(why)
	}
}

func TestSkeletonAndNameProblemAgreeOnEveryCharacter(t *testing.T) {
	// Characters that lower-case or fold to ASCII are problems, not just the ones in a table.
	for _, r := range []rune{0x212a, 0x0130, 0x017f, 0xa730, 0xfb02, 0x0192, 0x0430, 0x2011, 0x2215, 0x1d41a, 0x24d0, 0xff41, 0x13aa, 0x0581, 0x1806, 0x2e3a} {
		if NameProblem("a"+string(r)) == "" {
			t.Errorf("U+%04X passes for ASCII but is not a problem", r)
		}
	}
	for _, r := range []rune{'é', 'É', 'ß', 'я', 'д', '中', 'ø', 'ñ'} { // letters of their own
		if why := NameProblem("a" + string(r)); why != "" {
			t.Errorf("%c: %s", r, why)
		}
	}
	// The property, over every character the folds look at: no problem means the skeleton is the
	// name lower-cased, and the other way round.
	for r := rune(0x80); r < 0x30000; r++ {
		if r >= 0xd800 && r <= 0xdfff {
			continue
		}
		if _, accented := accentTable[unicode.ToLower(r)]; accented {
			continue // the skeleton (not the lint) reads an accented letter as its base letters, on purpose
		}
		name := "a" + string(r)
		sk, ok := Skeleton(name), NameProblem(name) == ""
		if ok && sk != "a"+string(unicode.ToLower(r)) {
			t.Fatalf("U+%04X: no problem, but its skeleton is %+q", r, sk)
		}
		if !ok && sk == "a"+string(r) {
			t.Fatalf("U+%04X: a problem, but nothing changes in its skeleton", r)
		}
	}
}

func TestNameProblemFindsWhatPassesForAnotherName(t *testing.T) {
	for name, want := range map[string]string{
		"laptop-a/claude.a1b2": "",
		"Laptop Pro/claude":    "", // plain upper case and a plain space are not lookalikes
		"laptop/claude.аб":     "U+0430",
		"mac‐14/claude":        "U+2010",
		"laptop/cl​aude":       "invisible",
		"laptop/‮claude":       "invisible",
		"laptop /claude":       "invisible",
		"laptop/clㅤaude":       "invisible",
		"laptop/cláude":       "combining",
		"laptop/⠀":             "invisible",
		"école/claude":         "", // other non-ASCII letters are allowed
	} {
		got := NameProblem(name)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q: %q", name, got)
		}
	}
}

func TestSafeNameWritesWhatPassesForAnotherNameOutLoud(t *testing.T) {
	for in, want := range map[string]string{
		"laptop/claude.a1b2": "laptop/claude.a1b2",
		"laptop‑x/claude":    "laptop<U+2011>x/claude",
		"maс/claude":         "ma<U+0441>/claude",
		"mac/cl​aude":        "mac/cl<U+200B>aude",
		"mac/clㅤaude":        "mac/cl<U+3164>aude",
		"mac/cl\x1b[2Jaude":  "mac/cl?[2Jaude",
		"aivoicebᴜs/x":       "aivoiceb<U+1D1C>s/x",
		"ㅤс":                 "<U+3164>с", // wholly foreign: only the invisible one is written out
	} {
		if got := SafeName(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestPersistentNamesCannotBeAutomaticOnes(t *testing.T) {
	for name, ok := range map[string]bool{
		"codex.build1":      true,
		"claude.review":     true,
		"mybot.nightly_1":   true,
		"codex.a1b2":        false, // four characters: the shape of an automatic session
		"claude.9058":       false,
		"codex":             false,
		"Codex.build1":      false,
		"codex.build 1":     false,
		"codex.bуild1":      false,
		"aivoicebus.build1": false,
		"codex.system":      false,
		"mcp.build1":        false,
		"codex.build1.x":    false,
	} {
		if err := ValidPersistentName(name); (err == nil) != ok {
			t.Errorf("%q: %v", name, err)
		}
	}
}

func TestRoleNamesAreNotTargetSyntax(t *testing.T) {
	for name, ok := range map[string]bool{"build": true, "review": true, "ci-1": true, "here": false, "all": false, "": false, "Build": false, "a=b": false, "repo": false, "role": false} {
		if ValidRoleName(name) != ok {
			t.Errorf("%q", name)
		}
	}
}

func TestEveryToolTheIdentityKnowsIsReserved(t *testing.T) {
	for _, tool := range []string{"claude", "codex", "gemini", "grok", "cursor-agent", "opencode", "aider", "goose"} {
		found := false
		for _, n := range ToolNames {
			found = found || n == tool
		}
		if !found {
			t.Errorf("%s is missing from ToolNames", tool)
		}
	}
}

func TestSkeletonFoldsLettersDigitsAndPunctuationToWhatTheyPassFor(t *testing.T) {
	for in, want := range map[string]string{
		"ᴀɪᴠᴏɪᴄᴇʙᴜꜱ":                     "aivoicebus",
		"①⑳":                             "120", // ① ⑳
		"⑴⒇":                             "120", // ⑴ ⒇
		"⓪":                              "0",
		"⓫⓴":                             "1120",
		"❶❿":                             "110",
		"\U0001d7ce\U0001d7d8\U0001d7ff": "009",
		"０９":                             "09",
		"⁰₉":                             "09",
		"﹒﹣":                             ".-",
		"․。":                             "..",
		"ⅠⅡ":                             "iii", // Ⅰ Ⅱ
		"\ufb00\ufb03\ufb06\u0133":       "ffffist" + "ij",
		"\u24b6\U0001f130":               "aa",
		"AK":                             "ak",
		"İ":                              "i",
	} {
		if got := Skeleton(in); got != want {
			t.Errorf("%+q -> %q, want %q", in, got, want)
		}
	}
}

func TestAForeignCharacterCannotHideAToolNameOrTheNodesMachine(t *testing.T) {
	// not in any table: only its removal (the projection) shows what the name is made to resemble
	for _, name := range []string{"laptop/cléaude.a1b2", "laptop/co中dex.q1w2"} {
		if SenderProblem(name) == "" || TargetProblem(name) == "" {
			t.Errorf("%+q", name)
		}
	}
	if !IsSystemSender("aivoi中cebus/x") {
		t.Error("the nodes' machine with a character taken out")
	}
}

func TestTheHostAndTheAgentAreLintedApart(t *testing.T) {
	for _, name := range []string{"сервер/claude.a1b2", "école/claude.a1b2", "中文/codex.q1w2"} {
		if why := NameProblem(name); why != "" {
			t.Errorf("%+q: %s", name, why)
		}
		if got := SafeName(name); got != name {
			t.Errorf("%+q shown as %q", name, got)
		}
	}
	if NameProblem("сервер/clаude.a1b2") == "" {
		t.Error("a lookalike inside the agent part is still found")
	}
}

// Blank characters that are not format controls are refused on their own, not only when they hide a
// reserved name (a combining one is dropped from the skeleton, so only the refusal catches it).
func TestEveryBlankCharacterIsRefusedInAnyName(t *testing.T) {
	for _, r := range []rune{0x2800, 0x3164, 0x115f, 0x1160, 0xffa0, 0x17b4, 0x17b5, 0x180b, 0x180c, 0x180d, 0x180e, 0x180f, 0xfe00, 0xfe0f, 0xe0100, 0xe0001, 0x034f} {
		name := "laptop/cl" + string(r) + "aude.a1b2"
		if !HasInvisible(name) || !strings.Contains(SenderProblem(name), "invisible") || !strings.Contains(TargetProblem(name), "invisible") || !strings.Contains(NameProblem(name), "invisible") {
			t.Errorf("U+%04X", r)
		}
	}
}

// Spaces of every kind between the characters of the system sender's name still claim it (0.8.1
// refused all of these; fold keeps the plain space and ASCII controls, so they are dropped here).
func TestSpacesInsideTheSystemSendersNameStillClaimIt(t *testing.T) {
	for _, from := range []string{
		"aivoice bus/x", "aivoicebus /x", " aivoicebus/x", "a i v o i c e b u s/x", "aivoicebus\t/x", "aivoice\tbus/x",
		"aivoice bus/x", "aivoicebus　/x", "aivoice bus/x", "aivoice bus/x", "aivoicebus\r/x", "aivoicebus\n/x",
	} {
		if !IsSystemSender(from) {
			t.Errorf("%+q is not seen as the system sender", from)
		}
		if SenderProblem(from) == "" {
			t.Errorf("%+q would be sent", from)
		}
	}
	if !strings.Contains(SafeName("aivoice bus/x"), "<U+0020>") || SafeName("aivoicebus/x") != "aivoicebus/x" || SafeName("my box/claude") != "my box/claude" {
		t.Errorf("a disguised system sender shows its spaces, an honest name is unchanged: %q %q %q", SafeName("aivoice bus/x"), SafeName("aivoicebus/x"), SafeName("my box/claude"))
	}
}

// A precomposed accent is no lookalike-free zone: "cláude" is a tool name in disguise, while a real
// accented hostname is only ever shown as it is. A name entirely in fullwidth forms is marked too.
func TestAccentedLookalikesCollideButRealAccentedHostnamesDoNot(t *testing.T) {
	for _, from := range []string{"laptop/cláude.a1b2", "laptop/codèx.a1b2", "laptop/gémini.x", "clāude/x", "laptop/çlaude.a1b2", "laptop/clǍude"} {
		if from == "laptop/clǍude" {
			continue // U+01CD is Latin Extended-B: not in the table, nothing claimed
		}
		if SenderProblem(from) == "" {
			t.Errorf("%+q would be sent as a tool name", from)
		}
	}
	for _, from := range []string{"café/claude.a1b2", "école/claude.a1b2", "münchen-1/codex.q1", "señor/aider.x"} {
		if SenderProblem(from) != "" {
			t.Errorf("%+q is a real hostname: %s", from, SenderProblem(from))
		}
		if SafeName(from) != from {
			t.Errorf("an accent alone is not marked: %q", SafeName(from))
		}
	}
	full := "ｉｍａｃ/claude.a1b2" // ｉｍａｃ
	if !strings.Contains(SafeName(full), "<U+FF49>") || NameProblem(full) == "" {
		t.Errorf("a fullwidth-only host shows unmarked: %q %q", SafeName(full), NameProblem(full))
	}
}
