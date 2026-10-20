package config

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Messages held back from agents (msg.HoldReason), and the ones the person released. Ids and
// reasons only; the text stays on the nodes.

const (
	heldFile     = "held.jsonl"
	releasedFile = "released"
)

// Held is one message held back from agents.
type Held struct {
	ID     string    `json:"id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// A version in a thread's shared space (Kind "share"): its thread and name. ID is then the
	// first ten hex digits of the version's id.
	Kind   string `json:"kind,omitempty"`
	Thread string `json:"thread,omitempty"`
	Name   string `json:"name,omitempty"`
}

// NoteHeld records that message id was held back, once.
func NoteHeld(id, reason string) { noteHeld(Held{ID: id, Reason: reason}) }

// NoteHeldShare records that a version of name in thread's shared space was held back, once.
func NoteHeldShare(id, reason, thread, name string) {
	noteHeld(Held{ID: id, Reason: reason, Kind: "share", Thread: thread, Name: name})
}

func noteHeld(h Held) {
	id := h.ID
	for _, h := range ReadHeld() {
		if h.ID == id {
			return
		}
	}
	if os.MkdirAll(Dir(), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(Dir(), heldFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	h.At = time.Now().UTC()
	b, _ := json.Marshal(h)
	_, _ = f.Write(append(b, '\n'))
}

// ReadHeld lists what was held, oldest first.
func ReadHeld() []Held {
	f, err := os.Open(filepath.Join(Dir(), heldFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Held
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var h Held
		if json.Unmarshal(sc.Bytes(), &h) == nil {
			out = append(out, h)
		}
	}
	return out
}

// Released reports whether the person released id.
func Released(id string) bool {
	b, _ := os.ReadFile(filepath.Join(Dir(), releasedFile))
	for _, line := range splitLines(string(b)) {
		if line == id {
			return true
		}
	}
	return false
}

// Release lets agents see id.
func Release(id string) error {
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(Dir(), releasedFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(id + "\n")
	return err
}
