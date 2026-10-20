package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// refuse answers a request with text and code, and names the reason in msg.HeaderRefused so a
// client can say what to do next in its own words (an older client reads the text).
func refuse(w http.ResponseWriter, reason, text string, code int) {
	w.Header().Set(msg.HeaderRefused, reason)
	http.Error(w, text, code)
}

// rateReason is the reason word for a rate limit.
func rateReason(e *store.RateLimitError) string {
	switch {
	case e.Scope == "pair":
		return msg.RefusedRatePair
	case e.Scope == "machine":
		return msg.RefusedRateMachine
	case e.What != "":
		return msg.RefusedRateRetract
	}
	return msg.RefusedRateSender
}

// turnReason is the reason word for a store.ErrTurnLimit refusal.
func turnReason(err error) string {
	switch e := err.Error(); {
	case !errors.Is(err, store.ErrTurnLimit):
		return msg.RefusedBadRequest
	case strings.Contains(e, "deadline passed"):
		return msg.RefusedDeadline
	case strings.Contains(e, "looks like a loop"):
		return msg.RefusedLoop
	case strings.Contains(e, "character budget"):
		return msg.RefusedBudget
	}
	return msg.RefusedTurns
}
