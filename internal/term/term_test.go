package term

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/view"
)

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Auto, "auto": Auto, "AUTO": Auto, "always": Always, "Yes": Always, "1": Always, "never": Never, "off": Never, "0": Never} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("ParseMode(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := ParseMode("rainbow"); err == nil || !strings.Contains(err.Error(), "auto, always or never") {
		t.Errorf("ParseMode(rainbow): %v", err)
	}
}

func TestWhenColourIsDrawn(t *testing.T) {
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	tests := []struct {
		name string
		flag Mode
		env  func(string) string
		tty  bool
		want bool
	}{
		{"a terminal", Auto, env(), true, true},
		{"a pipe", Auto, env(), false, false},
		{"NO_COLOR", Auto, env("NO_COLOR", "1"), true, false},
		{"NO_COLOR empty is not set", Auto, env("NO_COLOR", ""), true, true},
		{"a dumb terminal", Auto, env("TERM", "dumb"), true, false},
		{"xterm", Auto, env("TERM", "xterm-256color"), true, true},
		{"--color=always into a pipe", Always, env(), false, true},
		{"--color=always beats NO_COLOR", Always, env("NO_COLOR", "1", "TERM", "dumb"), false, true},
		{"--color=never on a terminal", Never, env(), true, false},
		{"AIVOICEBUS_COLOR=always into a pipe", Auto, env("AIVOICEBUS_COLOR", "always"), false, true},
		{"AIVOICEBUS_COLOR=never on a terminal", Auto, env("AIVOICEBUS_COLOR", "never"), true, false},
		{"the flag beats AIVOICEBUS_COLOR", Never, env("AIVOICEBUS_COLOR", "always"), true, false},
		{"AIVOICEBUS_COLOR=auto defers to NO_COLOR", Auto, env("AIVOICEBUS_COLOR", "auto", "NO_COLOR", "1"), true, false},
		{"a typo in AIVOICEBUS_COLOR is not colour", Auto, env("AIVOICEBUS_COLOR", "alwayz"), false, false},
	}
	for _, tc := range tests {
		if got := Enabled(tc.flag, tc.env, tc.tty); got != tc.want {
			t.Errorf("%s: Enabled = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestIsTerminalIsFalseForABuffer(t *testing.T) {
	if IsTerminal(&bytes.Buffer{}) {
		t.Error("a buffer is not a terminal")
	}
}

// The palette is the 16 basic colours plus bold and faint: nothing a light or dark theme cannot show.
func TestPaletteUsesBasicAnsiOnly(t *testing.T) {
	want := map[view.Role]string{ // the opening sequence; the library chooses how it closes
		view.RoleHeading: "\x1b[1m",
		view.RoleID:      "\x1b[1m",
		view.RoleFrom:    "\x1b[32m",
		view.RoleDim:     "\x1b[2m",
		view.RoleTask:    "\x1b[1;35m",
		view.RoleUrgent:  "\x1b[1;31m",
		view.RoleWarn:    "\x1b[1;33m",
		view.RoleBad:     "\x1b[1;31m",
		view.RoleGood:    "\x1b[32m",
		view.RoleCmd:     "\x1b[1;36m",
	}
	closer := regexp.MustCompile("^\\x1b\\[[0-9;]+m$")
	for role, open := range want {
		got := (Palette{}).Paint(role, "x")
		if !strings.HasPrefix(got, open+"x") || !closer.MatchString(strings.TrimPrefix(got, open+"x")) {
			t.Errorf("role %d: %q, want %q, x, then a reset", role, got, open)
		}
	}
	if got := (Palette{}).Paint(view.RolePlain, "x"); got != "x" {
		t.Errorf("a plain role is drawn: %q", got)
	}
	if got := (Palette{}).Paint(view.RoleCmd, ""); got != "" {
		t.Errorf("nothing is drawn as an empty sequence: %q", got)
	}
}

// Text handed to the palette cannot carry a sequence of its own into the output.
func TestPaintDropsControlCharacters(t *testing.T) {
	got := (Palette{}).Paint(view.RoleID, "a\x1b[31mb\x07c\u009bd\ne")
	if strings.Count(got, "\x1b") != 2 || strings.ContainsAny(got, "\x07\u009b\n") {
		t.Errorf("control characters survived: %q", got)
	}
}

func TestWrapPassesWritesThroughAndCarriesThePalette(t *testing.T) {
	var b bytes.Buffer
	w := Wrap(&b)
	w.Write([]byte("hello\n"))
	if b.String() != "hello\n" {
		t.Errorf("a write was changed: %q", b.String())
	}
	if !view.StyleOf(w).On() || view.StyleOf(&b).On() {
		t.Error("only the wrapped writer carries a style")
	}
}
