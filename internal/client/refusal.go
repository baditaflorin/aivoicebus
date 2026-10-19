package client

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// reasonOf is why a node refused: the word it named in msg.HeaderRefused, or, for a node from
// before that header, the word its status and text add up to ("" when they say nothing known).
func reasonOf(code int, header, text string) string {
	if header != "" {
		return view.OneLine(header, 40)
	}
	has := func(s string) bool { return strings.Contains(text, s) }
	switch code {
	case http.StatusUnauthorized:
		return msg.RefusedToken
	case http.StatusForbidden:
		switch {
		case has("only signed messages"):
			return msg.RefusedUnsigned
		case has("may only act as"):
			return msg.RefusedDenied
		case has("read-only"), has("may not read this"):
			return msg.RefusedReadOnly
		case has("can retract it"):
			return msg.RefusedNotSender
		}
	case http.StatusConflict:
		switch {
		case has("deadline passed"):
			return msg.RefusedDeadline
		case has("looks like a loop"):
			return msg.RefusedLoop
		}
		return msg.RefusedTurns
	case http.StatusUnprocessableEntity:
		switch {
		case has("looks like it contains"):
			return msg.RefusedSecret
		case has("does not hold that message"):
			return msg.RefusedUnknown
		}
	case http.StatusTooManyRequests:
		switch {
		case has("between the same two sessions"):
			return msg.RefusedRatePair
		case has("from one machine"):
			return msg.RefusedRateMachine
		case has("retractions"):
			return msg.RefusedRateRetract
		}
		return msg.RefusedRateSender
	case http.StatusBadRequest:
		switch {
		case has("is a retraction notice"):
			return msg.RefusedNotice
		case has("reserved for a retraction notice"):
			return msg.RefusedReservedID
		}
		return msg.RefusedBadRequest
	case http.StatusRequestEntityTooLarge:
		return msg.RefusedTooLarge
	}
	return ""
}

// Refused is why the node refused, as one msg.Refused* word ("" when unknown): what it named, or
// what its status and text add up to (for a node from before the header, or a refusal made here).
func (e *StatusError) Refused() string {
	if e.Reason != "" {
		return e.Reason
	}
	return reasonOf(e.Code, "", e.Text)
}

// later is when to try again after a rate limit: the node's Retry-After, or "later".
func later(d time.Duration) string {
	if d <= 0 {
		return "later"
	}
	if d < time.Minute {
		return fmt.Sprintf("in %d seconds", max(1, int(d.Round(time.Second)/time.Second)))
	}
	if m := int(d.Round(time.Minute) / time.Minute); m > 1 {
		return fmt.Sprintf("in %d minutes", m)
	}
	return "in a minute"
}

// explain is what a refusal means and what to do next, in plain words; ok is false for a reason
// this client does not know, which is then shown as the node said it.
func (e *StatusError) explain(reason string) (string, bool) {
	said := view.OneLine(e.Text, 300)
	switch reason {
	case msg.RefusedToken:
		return "the node does not accept this machine's token, so nothing was done. Ask the user to check it: `aivoicebus doctor` says which token is in use", true
	case msg.RefusedDenied:
		return "refused: this machine's own token may act only as this machine (" + said + "). Send as this machine, or ask the user to run it where the shared token is", true
	case msg.RefusedReadOnly:
		return "refused: this token may not do this (" + said + "). Ask the user to run it where the shared token is", true
	case msg.RefusedUnsigned:
		return "the node takes only signed messages, and this one was unsigned or did not verify, so it was not sent. Run `aivoicebus update`, then send it again", true
	case msg.RefusedSecret:
		return "the node refused the message: " + said + ". Nothing was sent: send it again without the secret", true
	case msg.RefusedTurns:
		return "this conversation reached its turn limit (--max-turns), so the message was not sent. Close it with `aivoicebus reply --end --re ID`, or start a new one with `aivoicebus send`", true
	case msg.RefusedDeadline:
		return "this conversation's deadline (--deadline) passed, so the message was not sent. Close it with `aivoicebus reply --end --re ID`, or start a new one with `aivoicebus send`", true
	case msg.RefusedLoop:
		return "the message was not sent: this sender said the same thing several times in this thread, which looks like a loop. Stop and ask the user whether to go on, or close it with `aivoicebus reply --end --re ID`", true
	case msg.RefusedRateSender:
		return "this sender has used its messages for the hour (rate limit), so nothing was sent. Try again " + later(e.RetryAfter) + ", send fewer and shorter messages, or ask the user", true
	case msg.RefusedRateMachine:
		return "this machine has used its messages for the hour, all its sessions together (rate limit), so nothing was sent. Try again " + later(e.RetryAfter) + ", or ask the user", true
	case msg.RefusedRatePair:
		return "the message was not sent: two sessions are answering each other fast enough to look like a loop. Stop, and ask the user whether to go on (or try again " + later(e.RetryAfter) + ")", true
	case msg.RefusedRateRetract:
		return "this identity has used its retractions for the hour (rate limit), so nothing was taken back. Try again " + later(e.RetryAfter), true
	case msg.RefusedUnknown:
		return "the nodes do not hold that message: it may still be on its way, or the id is wrong. Try again in a minute, or find the id with `aivoicebus inbox --all`", true
	case msg.RefusedNotSender:
		return "refused: " + said + ". Ask its sender to retract it", true
	case msg.RefusedReservedID:
		return "that message id belongs to a retraction notice, so nothing was sent. Send it again without an id (`aivoicebus send` picks one)", true
	case msg.RefusedNotice:
		return "that id is a retraction notice, not a message: there is nothing to reply to. Find the message with `aivoicebus inbox --all` and reply to that", true
	case msg.RefusedBadRequest:
		if view.HasNextStep(said) {
			return "the node refused the request: " + said, true
		}
		return "the node refused the request: " + said + ". This aivoicebus and the node may be different releases: run `aivoicebus update`, then try again", true
	case msg.RefusedTooLarge:
		return "the node refused the file: " + said + ". Send a smaller file, or ask the user", true
	case msg.RefusedGone:
		return "the file is gone: it expired or was already read. Ask its sender to send it again", true
	case msg.RefusedRecovering:
		return "the node is catching up with its peers. Try again in a minute", true
	}
	return "", false
}

// generic is the next step for a refusal whose reason this client does not know, by its status.
func (e *StatusError) generic() string {
	if e.Code == http.StatusNotFound {
		return "The node may run an older release that lacks this: ask the user to update the nodes (`aivoicebus nodes` lists them)"
	}
	return "If it persists, run `aivoicebus doctor` and ask the user"
}

// explained is e.Error() for a refusal this client knows: the plain words with the node's reason.
func (e *StatusError) explained(reason string) string {
	if s, ok := e.explain(reason); ok {
		return s + "."
	}
	s := view.OneLine(e.Status, 60) + ": " + view.OneLine(e.Text, 300)
	if !view.HasNextStep(s) {
		s = strings.TrimRight(s, ". ") + ". " + e.generic() + "."
	}
	return s
}
