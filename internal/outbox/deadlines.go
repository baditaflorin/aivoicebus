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

// The deadlines this machine's senders asked for (send --reply-by, --until) are remembered in
// ~/.config/aivoicebus/deadlines.json, next to sent.json and in the same spirit: ids, identities
// and times, never a body, and nothing on the wire. The sender's machine judges them from the receipts
// the nodes report (msg.JudgeDeadline), by `send` while it waits, by `watch` for one that passed with
// nobody waiting, and `status ID` shows the verdict on request. A file that cannot be read or written
// costs the deadline report, never the message.
const (
	deadlinesFile    = "deadlines.json"
	deadlinesLock    = "deadlines.lock"
	deadlinesKeep    = 50                 // per machine
	deadlinesReport  = 7 * 24 * time.Hour // a reported deadline is kept this long, for `status`
	deadlinesForget  = 30 * 24 * time.Hour
	maxDeadlineWords = 200 // a target is kept to this many bytes: a selector can be long, and the file is read back as data
)

// Deadline is one remembered deadline.
type Deadline struct {
	ID     string    `json:"id"`
	Thread string    `json:"thread"`
	From   string    `json:"from"`
	To     string    `json:"to"`
	Kind   string    `json:"kind"` // msg.DeadlineReply or msg.DeadlineTake
	Sent   time.Time `json:"sent"`
	By     time.Time `json:"by"`
	// Reach is the sender's count of recipients when it sent it (msg.Message.Reach), kept so a report
	// can say "of 3" even when the nodes forgot the message's count.
	Reach *int `json:"reach,omitempty"`
	// Reminders are the ids of the messages escalation sent for this one: what reads them is late.
	Reminders []string `json:"reminders,omitempty"`
	// Reported is when a report was made (the waiting send, or watch); zero until then.
	Reported time.Time `json:"reported,omitzero"`
}

// deadlineBook is the file: each machine and its deadlines, oldest first.
type deadlineBook map[string][]Deadline

// usable reports whether d is shaped like what was written; the file is read back as data.
func (d Deadline) usable(now time.Time) bool {
	if d.Kind != msg.DeadlineReply && d.Kind != msg.DeadlineTake {
		return false
	}
	if !msg.ValidID(d.ID) || !msg.ValidID(d.Thread) || !msg.ValidIdentity(d.From) || len(d.To) == 0 || len(d.To) > maxDeadlineWords {
		return false
	}
	if d.Sent.IsZero() || d.By.IsZero() || d.By.Before(d.Sent) || d.By.Sub(d.Sent) > msg.MaxSenderDeadline+time.Minute {
		return false
	}
	if !d.Reported.IsZero() && now.Sub(d.Reported) > deadlinesReport {
		return false
	}
	if now.Sub(d.By) > deadlinesForget {
		return false
	}
	for _, id := range d.Reminders {
		if !msg.ValidID(id) {
			return false
		}
	}
	return true
}

func readDeadlines(now time.Time) deadlineBook {
	book := deadlineBook{}
	dir := config.Dir()
	if dir == "" {
		return book
	}
	raw, err := os.ReadFile(filepath.Join(dir, deadlinesFile))
	var in deadlineBook
	if err != nil || json.Unmarshal(raw, &in) != nil {
		return book
	}
	for host, list := range in {
		for _, d := range list {
			if d.usable(now) && msg.HostOf(d.From) == host {
				book[host] = append(book[host], d)
			}
		}
		if n := len(book[host]); n > deadlinesKeep {
			book[host] = book[host][n-deadlinesKeep:]
		}
	}
	return book
}

// changeDeadlines reads the book under its lock, lets edit change it, and writes it back. It reports
// whether it could: another process holding the lock for more than a moment, or an unwritable
// directory, is "no", and nothing was changed.
func changeDeadlines(edit func(book deadlineBook)) bool {
	dir := config.Dir()
	if dir == "" || os.MkdirAll(dir, 0o700) != nil {
		return false
	}
	unlock, ok := lockSent(filepath.Join(dir, deadlinesLock), 2*sentLockWait)
	if !ok {
		return false
	}
	defer unlock()
	book := readDeadlines(clock())
	edit(book)
	return writeAtomic(dir, deadlinesFile, "deadlines-*.tmp", book)
}

// RememberDeadline notes d, replacing an earlier entry for the same message (a repeated send is one
// message). It reports whether it was written.
func RememberDeadline(d Deadline) bool {
	host := msg.HostOf(d.From)
	if !d.usable(clock()) {
		return false
	}
	return changeDeadlines(func(book deadlineBook) {
		list := slices.DeleteFunc(book[host], func(e Deadline) bool { return e.ID == d.ID })
		list = append(list, d)
		if len(list) > deadlinesKeep {
			list = list[len(list)-deadlinesKeep:]
		}
		book[host] = list
	})
}

// DeadlinesFrom lists the deadlines the machine host remembers, oldest first.
func DeadlinesFrom(host string) []Deadline { return readDeadlines(clock())[host] }

// DeadlineFor is the deadline remembered for message id on machine host.
func DeadlineFor(host, id string) (Deadline, bool) {
	for _, d := range DeadlinesFrom(host) {
		if d.ID == id {
			return d, true
		}
	}
	return Deadline{}, false
}

// UpdateDeadline changes the remembered deadline of id (on machine host) with edit and reports whether
// there was one and it was written.
func UpdateDeadline(host, id string, edit func(*Deadline)) bool {
	found := false
	ok := changeDeadlines(func(book deadlineBook) {
		for i := range book[host] {
			if book[host][i].ID == id {
				edit(&book[host][i])
				found = true
			}
		}
	})
	return ok && found
}

// MarkReported notes that deadline id was reported at now, so `watch` does not report it again.
func MarkReported(host, id string, now time.Time) bool {
	return UpdateDeadline(host, id, func(d *Deadline) { d.Reported = now.UTC() })
}

// AddReminders notes the reminders sent for deadline id.
func AddReminders(host, id string, ids ...string) bool {
	return UpdateDeadline(host, id, func(d *Deadline) {
		for _, r := range ids {
			if !slices.Contains(d.Reminders, r) {
				d.Reminders = append(d.Reminders, r)
			}
		}
	})
}
