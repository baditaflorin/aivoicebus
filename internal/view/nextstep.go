package view

import (
	"regexp"
	"strings"
)

// nextStep matches the words that tell a reader what to do next: a command in backticks, an
// aivoicebus command, asking someone (the user, the sender), or doing it again.
var nextStep = regexp.MustCompile("`[^`]+`" +
	`|\baivoicebus [a-z]` +
	`|(?i:\bask (the|its|whoever|them|a person|the person|the machine)\b)` +
	`|(?i:\b(try|run|send|call) (it |this |that )?again\b|\bretry\b)`)

// HasNextStep reports whether an error's text tells its reader what to do next: a command to run,
// or someone to ask. A usage line is one (it is the command to type). Every error a person or an
// agent reads must have one (errors_test.go keeps the rule).
func HasNextStep(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "usage:") || nextStep.MatchString(s)
}
