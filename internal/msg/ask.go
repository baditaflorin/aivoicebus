package msg

import (
	"regexp"
	"strings"
)

// ask-human: an agent asks the person a question and waits. The question goes to <machine>/human
// (no agent session reads that identity), with its choices, and, when the person has a phone
// channel, the hash of a one-time code that the phone's choice buttons carry (see decide.go).

const AskPrefix = "[ask-human] "

var choiceWord = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// HumanOf is the identity a machine's person is asked as.
func HumanOf(me string) string { return HostOf(me) + "/human" }

// AskBody is the question message.
func AskBody(question string, choices []string, code string) string {
	b := AskPrefix + question
	if len(choices) > 0 {
		b += "\nchoices: " + strings.Join(choices, "|")
	}
	if code != "" {
		b += "\n" + PhoneCheckLine(code)
	}
	return b
}

// AskChoices are the choices a question offers (none: a free answer).
func AskChoices(body string) []string {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, "choices: "); ok {
			var out []string
			for _, c := range strings.Split(rest, "|") {
				if choiceWord.MatchString(c) {
					out = append(out, c)
				}
			}
			return out
		}
	}
	return nil
}

// ValidChoice reports whether c is a choice word (lowercase letters, digits, - and _).
func ValidChoice(c string) bool { return choiceWord.MatchString(c) }

// HumanAnswerPrefix starts an answer typed by the person (aivoicebus answer).
const HumanAnswerPrefix = "[human] "
