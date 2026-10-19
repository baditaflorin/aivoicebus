package msg

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A conversation state is the handshake two agents run on a thread, said out loud so both sides
// and the viewer see the same thing:
//
//	offered -> taken -> ready -> doing -> done
//
// with stuck (something is in the way; it can go on to ready, doing or done) and declined (it will
// not be done). done and declined are final. The thread's sender offers it; whoever takes it moves
// it on. A task is offered by being sent, and taken by its first claim.
//
// A state travels as a progress message whose first line starts with its tag ("[state ready]
// approved by the person"): every node since ProgressSince stores and replicates that unchanged, and
// a client that predates states shows an ordinary progress message. The state of a thread is what
// ThreadState folds from its messages; a move the rules refuse is ignored there, so a peer cannot
// skip the handshake by writing the tag.
const (
	StateOffered  = "offered"
	StateTaken    = "taken"
	StateReady    = "ready"
	StateDoing    = "doing"
	StateDone     = "done"
	StateStuck    = "stuck"
	StateDeclined = "declined"

	// StatesSince is the release whose nodes keep a state message out of the pair brake and the turn
	// count, and let done, stuck and declined through a used-up turn limit or deadline (FeatureStates).
	// Older nodes hold it as the progress message it is.
	StatesSince   = "0.11.0"
	FeatureStates = "states"

	stateTagOpen = "[state "
	MaxStateNote = 400 // runes of a state's note shown
)

// States in the order help and schemas list them.
var States = []string{StateOffered, StateTaken, StateReady, StateDoing, StateDone, StateStuck, StateDeclined}

// StateChoices is the states as a flag's help writes them.
func StateChoices() string { return strings.Join(States, "|") }

// ValidState reports whether s is a state.
func ValidState(s string) bool {
	for _, x := range States {
		if x == s {
			return true
		}
	}
	return false
}

// rank is a state's place on the main line; stuck and declined are off it (-1).
func rank(s string) int {
	switch s {
	case "":
		return -1
	case StateOffered:
		return 0
	case StateTaken:
		return 1
	case StateReady:
		return 2
	case StateDoing:
		return 3
	case StateDone:
		return 4
	}
	return -1
}

// FinalState reports whether nothing comes after s.
func FinalState(s string) bool { return s == StateDone || s == StateDeclined }

// ClosingState reports whether s may close a conversation whose turn limit or deadline is used up,
// like an [end]: the last word of work that was going on.
func ClosingState(s string) bool { return s == StateDone || s == StateStuck || s == StateDeclined }

// StateBody is the body of a message that moves a thread to state, with an optional note.
func StateBody(state, note string) string {
	note = strings.TrimSpace(note)
	if note == "" {
		return stateTagOpen + state + "]"
	}
	return stateTagOpen + state + "] " + note
}

// StateOf reports the state m moves its thread to and its note: m is a progress message whose
// first line starts with a state tag. A body sealed for one machine says nothing to anyone else.
func StateOf(m Message) (state, note string, ok bool) {
	if m.Kind != KindProgress {
		return "", "", false
	}
	return parseStateBody(m.Body)
}

// IsStateBody reports whether body, sent as a progress message, moves a thread's state. Nodes use it
// to keep state messages out of the pair brake and the turn count.
func IsStateBody(body string) bool { _, _, ok := parseStateBody(body); return ok }

func parseStateBody(body string) (string, string, bool) {
	rest, ok := strings.CutPrefix(body, stateTagOpen)
	if !ok {
		return "", "", false
	}
	name, rest, ok := strings.Cut(rest, "]")
	if !ok || !ValidState(name) {
		return "", "", false
	}
	return name, strings.TrimSpace(rest), true
}

// StateStep is one move of a thread's state.
type StateStep struct {
	State string    `json:"state"`
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	ID    string    `json:"id"`             // the message that moved it (the task, a claim, a state message)
	Note  string    `json:"note,omitempty"` // what the mover said with it
}

// StateIgnored is a state message the rules did not take, and why.
type StateIgnored struct {
	StateStep
	Why string `json:"why"`
}

// StateView is a thread's state as its messages make it.
type StateView struct {
	State   string         `json:"state"` // "" when nobody offered or took anything in it
	By      string         `json:"by,omitempty"`
	At      time.Time      `json:"at,omitzero"`
	Note    string         `json:"note,omitempty"`
	Offerer string         `json:"offerer,omitempty"` // who started the thread
	Taker   string         `json:"taker,omitempty"`   // who took it: the one who moves it on
	History []StateStep    `json:"history,omitempty"`
	Ignored []StateIgnored `json:"ignored,omitempty"`
	Root    string         `json:"root,omitempty"` // the thread's first message
}

// Final reports whether the thread's state can no longer change.
func (v StateView) Final() bool { return FinalState(v.State) }

// Path is the states it went through, "offered -> taken -> ready".
func (v StateView) Path() string {
	out := make([]string, 0, len(v.History))
	for _, h := range v.History {
		out = append(out, h.State)
	}
	return strings.Join(out, " -> ")
}

// Reached reports whether the thread got to want: it is there, or further on the main line (a wait
// for ready is over once it is doing or done). stuck and declined reach only themselves.
func (v StateView) Reached(want string) bool {
	if v.State == want {
		return true
	}
	return rank(want) >= 0 && rank(v.State) > rank(want)
}

// CanReach reports whether the thread may still get to want: false once it is final without being
// there.
func (v StateView) CanReach(want string) bool {
	return v.Reached(want) || !v.Final()
}

// CheckMove says why who may not move the thread to state, or nil. The words are for the agent that
// tried, so each says what to do instead.
func (v StateView) CheckMove(who, state string) error {
	switch {
	case !ValidState(state):
		return fmt.Errorf("%q is not a state; the states are %s", state, StateChoices())
	case v.Final():
		return fmt.Errorf("the conversation is %s already (said by %s at %s): a %s conversation does not change; start a new one", v.State, v.By, v.At.Local().Format("15:04:05"), v.State)
	case state == v.State:
		return fmt.Errorf("it is %s already (said by %s at %s)", state, v.By, v.At.Local().Format("15:04:05"))
	case state == StateOffered:
		if who != v.Offerer {
			return fmt.Errorf("only %s, who started the conversation, offers it; you take it with: taken", v.Offerer)
		}
		if v.State != "" {
			return fmt.Errorf("it is %s already: offered is how a conversation starts", v.State)
		}
		return nil
	case who == v.Offerer:
		return fmt.Errorf("you offered it: whoever takes it moves it on (taken, ready, doing, done, stuck or declined). To call it off, retract it or close it with: reply --end")
	case v.Taker != "" && who != v.Taker:
		return fmt.Errorf("%s took it: only they move it on now", v.Taker)
	case state == StateStuck, state == StateDeclined:
		return nil
	case v.State == StateStuck:
		if state == StateTaken {
			return fmt.Errorf("it is taken already (stuck now): go on with ready, doing or done, or say declined")
		}
		return nil
	case rank(state) <= rank(v.State):
		return fmt.Errorf("a state only moves forward (offered -> taken -> ready -> doing -> done) and it is %s: say stuck if something is in the way, or declined if it will not be done", v.State)
	}
	return nil
}

// ThreadState folds a thread's messages into its state. The thread's first message names who
// offered it; a task is offered by being sent; the first claim or answer to it (as TaskOwner weighs
// them) takes it; state messages move it as CheckMove allows, and the others are listed as ignored.
// Withdrawn messages count for nothing.
func ThreadState(ms []Message) StateView {
	var v StateView
	if len(ms) == 0 {
		return v
	}
	ms = append([]Message(nil), ms...)
	sort.SliceStable(ms, func(i, j int) bool {
		if !ms[i].At.Equal(ms[j].At) {
			return ms[i].At.Before(ms[j].At)
		}
		return ms[i].ID < ms[j].ID
	})
	root := ms[0]
	for _, m := range ms {
		if m.ID == m.Thread {
			root = m
			break
		}
	}
	v.Root, v.Offerer = root.ID, root.From
	move := func(m Message, state, note string) {
		v.State, v.By, v.At, v.Note = state, m.From, m.At, note
		if state != StateOffered && v.Taker == "" && m.From != v.Offerer {
			v.Taker = m.From
		}
		v.History = append(v.History, StateStep{State: state, By: m.From, At: m.At, ID: m.ID, Note: note})
	}
	if root.Kind == KindTask && root.Retracted == nil {
		move(root, StateOffered, "")
	}
	for _, m := range ms {
		if m.Retracted != nil || m.ID == root.ID && root.Kind == KindTask {
			continue
		}
		if state, note, ok := StateOf(m); ok {
			if err := v.CheckMove(m.From, state); err != nil {
				v.Ignored = append(v.Ignored, StateIgnored{StateStep{State: state, By: m.From, At: m.At, ID: m.ID, Note: note}, err.Error()})
				continue
			}
			move(m, state, note)
			continue
		}
		// a claim or an answer to the task takes it, while nobody has
		if Takes(m) && m.Re == root.ID && m.From != v.Offerer && v.Taker == "" && (v.State == "" || v.State == StateOffered) {
			move(m, StateTaken, "")
		}
	}
	return v
}
