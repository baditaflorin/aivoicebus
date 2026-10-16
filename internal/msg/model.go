package msg

import (
	"regexp"
	"strings"
)

// ModelLabel is the label that says which model a session runs ("claude-opus-5-5"), so `who`, the
// viewer and selectors (@model=claude-opus-*) can tell sessions apart by it. Like every label it is
// self-declared.
const ModelLabel = "model"

var modelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// CleanModel is s as a model id if it looks like one (lowercase letters, digits, dot, dash and
// underscore, starting with a letter or digit, at most 64 characters), else "". Placeholders such as
// "<synthetic>" are not models and give "".
func CleanModel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if !modelRe.MatchString(s) {
		return ""
	}
	return s
}

// replMaxModel bounds a replicated message's model in bytes; a reader cleans it again.
const replMaxModel = 64
