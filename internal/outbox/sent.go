package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The messages this machine sent lately are remembered in ~/.config/aivoicebus/sent.json, so that
// `aivoicebus retract` with no id can take back the last one. The file holds ids, senders, targets
// and times and never a body: it must not become a second copy of what was said. It is a
// convenience, so a file that cannot be read, parsed or written is ignored, never an error: the
// worst case is that retract asks for an id.
const (
	sentFile       = "sent.json"
	sentLockFile   = "sent.lock"    // next to it: held while the file is read, changed and replaced
	sentKeep       = 10             // messages remembered per machine
	sentWindow     = 24 * time.Hour // older ones are forgotten
	sentWindowText = "24h"
)

// clock is the memory's time source; tests replace it.
var clock = time.Now

// sentLockWait is how long a send waits for another to finish remembering before it skips
// remembering its own; tests change it.
var sentLockWait = 200 * time.Millisecond

// sentMsg is one remembered message.
type sentMsg struct {
	ID   string    `json:"id"`
	From string    `json:"from"`
	To   string    `json:"to"`
	At   time.Time `json:"at"`
}

// sentBook is the file: each machine (the host part of the sender) and its messages, oldest first.
// It is keyed by machine because the authority to retract is the machine's, and so that a changed
// AIVOICEBUS_HOST does not mix two machines' lists.
type sentBook map[string][]sentMsg

// usable reports whether e is a message worth offering for retraction at now. The file is the
// user's own, but it is read back as data: an entry that is not shaped like what was written (a
// hand edit, another version) is dropped rather than trusted.
func (e sentMsg) usable(now time.Time) bool {
	return msg.ValidRetractID(e.ID) && msg.ValidIdentity(e.From) && msg.ValidIdentity(e.To) &&
		e.At.After(now.Add(-sentWindow)) && !e.At.After(now.Add(time.Minute))
}

// readSent loads the file, keeping only the usable entries of each machine, at most sentKeep. A
// missing or corrupt file is an empty book.
func readSent(now time.Time) sentBook {
	book := sentBook{}
	dir := config.Dir()
	if dir == "" {
		return book
	}
	raw, err := os.ReadFile(filepath.Join(dir, sentFile))
	var in sentBook
	if err != nil || json.Unmarshal(raw, &in) != nil {
		return book
	}
	for host, list := range in {
		for _, e := range list {
			if e.usable(now) && msg.HostOf(e.From) == host {
				book[host] = append(book[host], e)
			}
		}
		if n := len(book[host]); n > sentKeep {
			book[host] = book[host][n-sentKeep:]
		}
	}
	return book
}

// rememberSent notes that m, which a node just accepted, was sent from this machine.
func rememberSent(m msg.Message) {
	dir := config.Dir()
	if dir == "" || !msg.ValidRetractID(m.ID) {
		return
	}
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	unlock, ok := lockSent(filepath.Join(dir, sentLockFile), sentLockWait)
	if !ok {
		return // another send holds it, or it cannot be taken: not worth holding this one up
	}
	defer unlock()
	now := clock()
	book := readSent(now) // also forgets whatever is stale, on every machine
	host := msg.HostOf(m.From)
	list := slices.DeleteFunc(book[host], func(e sentMsg) bool { return e.ID == m.ID }) // a repeated send is one message
	list = append(list, sentMsg{ID: m.ID, From: m.From, To: m.To, At: now.UTC()})
	if len(list) > sentKeep {
		list = list[len(list)-sentKeep:]
	}
	book[host] = list
	writeSent(dir, book)
}

// writeSent replaces the file atomically (mode 0600), so a reader never sees half of one and a
// crash leaves the old one.
func writeSent(dir string, book sentBook) { writeAtomic(dir, sentFile, "sent-*.tmp", book) }

// writeAtomic writes v as JSON to dir/name through a temporary file of the given pattern and a
// rename, mode 0600, and reports whether it all worked.
func writeAtomic(dir, name, pattern string, v any) bool {
	b, err := json.Marshal(v)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return false
	}
	if tmp.Chmod(0o600) != nil || tmp.Close() != nil {
		return false
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name)) == nil
}

// lastSent is the message who takes back when it names none: the latest it sent as exactly this
// identity (exact), else the latest sent from its machine, which another session may have sent
// (a session that restarted has a new suffix, but so does every other session on the machine).
// With machine set it is the machine's latest whoever sent it, which is what the person at a
// terminal means by "whatever this machine sent last".
func lastSent(who string, now time.Time, machine bool) (e sentMsg, exact, ok bool) {
	var host, mine *sentMsg
	list := readSent(now)[msg.HostOf(who)]
	for i := range list {
		if host == nil || !list[i].At.Before(host.At) {
			host = &list[i]
		}
		if list[i].From == who && (mine == nil || !list[i].At.Before(mine.At)) {
			mine = &list[i]
		}
	}
	switch {
	case mine != nil && !machine:
		return *mine, true, true
	case host != nil:
		return *host, host.From == who, true
	}
	return sentMsg{}, false, false
}

// SentBy is the ids of the messages sent as exactly who from this machine within sentWindow
// (the last sentKeep of them). The per-prompt hook shows a reply mid-session only when it
// answers one of these: a message that merely says "re" something is not shown to a session
// that never sent it. A session that sent more than sentKeep messages since, or whose
// memory could not be read, simply sees the reply at its next start.
func SentBy(who string) map[string]bool {
	ids := map[string]bool{}
	for _, e := range readSent(clock())[msg.HostOf(who)] {
		if e.From == who {
			ids[e.ID] = true
		}
	}
	return ids
}

// SentRef is one message this machine sent lately, as sent.json remembers it.
type SentRef struct {
	ID, From, To string
	At           time.Time
}

// SentFrom lists what the machine host sent lately (the last few, at most a day old), oldest first.
func SentFrom(host string) []SentRef {
	var out []SentRef
	for _, e := range readSent(clock())[host] {
		out = append(out, SentRef{ID: e.ID, From: e.From, To: e.To, At: e.At})
	}
	return out
}
