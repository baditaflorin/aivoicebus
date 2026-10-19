package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// maxUnsignedDays bounds the window of /v1/unsigned: the longest a message is meant to matter.
const maxUnsignedDays = 30

// unsignedHandler answers GET /v1/unsigned?days=N (default 7) with who sent messages without a valid
// signature in the last N days (msg.UnsignedReport): the messages `serve --require-signed` would
// have refused. Join and selftest probes are left out, as that switch lets them through.
func unsignedHandler(s *store.Store, node string, requireSigned bool, now func() time.Time) principalHandler {
	return func(w http.ResponseWriter, r *http.Request, _ Principal) {
		days := 7
		if v := r.URL.Query().Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > maxUnsignedDays {
				http.Error(w, "bad request: days is a whole number from 1 to 30", http.StatusBadRequest)
				return
			}
			days = n
		}
		since := now().Add(-time.Duration(days) * 24 * time.Hour)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(unsignedReport(s.ArrivedSince(since), node, requireSigned, since))
	}
}

// unsignedReport counts, per sender, the messages in ms that carry no signature or one that does not
// verify. The signatures are checked here, outside the store's lock.
func unsignedReport(ms []msg.Message, node string, requireSigned bool, since time.Time) msg.UnsignedReport {
	rep := msg.UnsignedReport{Node: node, Since: since.UTC(), RequireSigned: requireSigned, Senders: []msg.UnsignedSender{}}
	by := map[string]*msg.UnsignedSender{}
	for _, m := range ms {
		if msg.IsProbe(m.From) {
			continue
		}
		rep.Scanned++
		signed, valid := m.VerifySig()
		if signed && valid {
			continue
		}
		e := by[m.From]
		if e == nil {
			e = &msg.UnsignedSender{From: m.From}
			by[m.From] = e
		}
		if signed {
			e.Bad++
		} else {
			e.Unsigned++
		}
		if !m.At.Before(e.LastAt) {
			e.LastAt, e.LastID = m.At, m.ID
		}
	}
	for _, e := range by {
		rep.Senders = append(rep.Senders, *e)
	}
	sort.Slice(rep.Senders, func(i, j int) bool { return rep.Senders[i].LastAt.After(rep.Senders[j].LastAt) })
	return rep
}
