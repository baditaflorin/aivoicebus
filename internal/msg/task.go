package msg

import "time"

// A task is work for ONE session among those its address reaches, usually every session on one
// machine (`aivoicebus task laptop-b "..."`). Every one of them is shown it until a session takes
// it; from then on only that session is. A session takes a task with a claim, or by answering it
// (an answer to a task is a claim too). Two sessions may claim at nearly the same time, on
// different nodes: the earliest claim wins, by the time the node stamped on it and then by id, so
// every node and every client names the same owner once it holds both claims.

// TakeRef is one claim (or answer) to a task, as TaskOwner weighs it.
type TakeRef struct {
	ID   string
	From string
	At   time.Time
}

// Takes reports whether m takes the task named by its Re: a claim or an answer.
func Takes(m Message) bool { return (m.Kind == KindClaim || m.Kind == KindAnswer) && m.Re != "" }

// Earlier orders claims: by time, then by id, so the order is the same everywhere.
func (a TakeRef) Earlier(b TakeRef) bool {
	if !a.At.Equal(b.At) {
		return a.At.Before(b.At)
	}
	return a.ID < b.ID
}

// TaskOwner is the session that holds a task sent by from, given the claims on it that still
// stand (not withdrawn): the earliest one not made by the task's own sender. False when nobody
// has taken it.
func TaskOwner(from string, takes []TakeRef) (TakeRef, bool) {
	var best TakeRef
	found := false
	for _, t := range takes {
		if t.From == from {
			continue
		}
		if !found || t.Earlier(best) {
			best, found = t, true
		}
	}
	return best, found
}
