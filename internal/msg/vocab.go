package msg

import "strings"

// Word is one value of a message's priority or kind, declared once: the CLI flags, the MCP
// schemas, the header tags and the validation all read these tables, and a test holds the docs
// to them, so a new value cannot be added in one place and forgotten in another.
type Word struct {
	Name string
	Tag  string // marker after a message header; "" for the default, which carries none
}

// Priorities in the order they are listed in help and schemas.
var Priorities = []Word{
	{PrioFYI, "[fyi]"},
	{PrioAsk, ""},
	{PrioUrgent, "[urgent]"},
}

// Kinds in the order they are listed in help and schemas. An empty kind (a plain note) is not a
// word: it is the absence of one.
var Kinds = []Word{
	{KindQuestion, "[question]"},
	{KindAnswer, "[answer]"},
}

func names(ws []Word) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = w.Name
	}
	return out
}

func has(ws []Word, s string) bool {
	for _, w := range ws {
		if w.Name == s {
			return true
		}
	}
	return false
}

// PrioNames and KindNames list the words, for an enum in a schema.
func PrioNames() []string { return names(Priorities) }
func KindNames() []string { return names(Kinds) }

// PrioChoices and KindChoices are the words as a flag's help and the docs write them: "fyi|ask|urgent".
func PrioChoices() string { return strings.Join(PrioNames(), "|") }
func KindChoices() string { return strings.Join(KindNames(), "|") }

// Tag is the header marker of a priority or kind word ("" when it has none or is not a word).
func Tag(ws []Word, name string) string {
	for _, w := range ws {
		if w.Name == name {
			return w.Tag
		}
	}
	return ""
}
