package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// What each standing-by session has already told its person about: a task meant for the person
// (msg.PersonOnly) is mentioned once per session, not on every wait. Ids and identities only.

const (
	toldFile = "person-told"
	toldKeep = 500 // lines kept: the newest
)

var toldMu sync.Mutex

// Told reports whether session me already told its person about message id.
func Told(me, id string) bool {
	b, _ := os.ReadFile(filepath.Join(Dir(), toldFile))
	for _, line := range splitLines(string(b)) {
		if line == me+" "+id {
			return true
		}
	}
	return false
}

// NoteTold records that session me told its person about message id.
func NoteTold(me, id string) {
	toldMu.Lock()
	defer toldMu.Unlock()
	if os.MkdirAll(Dir(), 0o700) != nil || strings.ContainsAny(me+id, " \n") {
		return
	}
	p := filepath.Join(Dir(), toldFile)
	b, _ := os.ReadFile(p)
	lines := append(splitLines(string(b)), me+" "+id)
	if len(lines) > toldKeep {
		lines = lines[len(lines)-toldKeep:]
	}
	f, err := os.CreateTemp(Dir(), ".person-told-*") // mode 0600; a name of its own, for two sessions at once
	if err != nil {
		return
	}
	_, werr := f.WriteString(strings.Join(lines, "\n") + "\n")
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), p) != nil {
		_ = os.Remove(f.Name())
	}
}
