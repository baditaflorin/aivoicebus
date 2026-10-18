package server

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// metrics counts what a node needs to be watched: requests per token name (so
// operators can see which tokens are still in use before dropping one) and
// refused requests. Everything else is read from the store and the peer board
// when /metrics is scraped.
type metrics struct {
	unauthorized atomic.Int64
	secrets      atomic.Int64
	unsigned     atomic.Int64 // sends refused by --require-signed
	mu           sync.Mutex
	requests     map[string]*atomic.Int64
}

func newMetrics(names []string) *metrics {
	m := &metrics{requests: map[string]*atomic.Int64{}}
	for _, n := range names {
		m.requests[n] = new(atomic.Int64)
	}
	return m
}

func (m *metrics) count(name string) {
	m.mu.Lock()
	c := m.requests[name]
	if c == nil {
		c = new(atomic.Int64)
		m.requests[name] = c
	}
	m.mu.Unlock()
	c.Add(1)
}

// label escapes a Prometheus label value.
func label(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// write renders the Prometheus text exposition of one node.
func (m *metrics) write(w io.Writer, h msg.Health, st store.Stats) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }
	p("# HELP aivoicebus_info The node and the release it runs.")
	p("# TYPE aivoicebus_info gauge")
	p(`aivoicebus_info{node="%s",version="%s"} 1`, label(h.Node), label(h.Version))
	p("# HELP aivoicebus_messages Messages this node holds.")
	p("# TYPE aivoicebus_messages gauge")
	p("aivoicebus_messages %d", st.Messages)
	p("# HELP aivoicebus_acks Read receipts this node holds.")
	p("# TYPE aivoicebus_acks gauge")
	p("aivoicebus_acks %d", st.Acks)
	p("# HELP aivoicebus_sessions Sessions this node has heard from.")
	p("# TYPE aivoicebus_sessions gauge")
	p("aivoicebus_sessions %d", st.Sessions)
	p("# HELP aivoicebus_duplicates_total Messages dropped as repeats since start.")
	p("# TYPE aivoicebus_duplicates_total counter")
	p("aivoicebus_duplicates_total %d", st.Duplicates)
	p("# HELP aivoicebus_rate_limited_total Sends refused by the per-sender rate limit since start.")
	p("# TYPE aivoicebus_rate_limited_total counter")
	p("aivoicebus_rate_limited_total %d", st.RateLimited)
	p("# HELP aivoicebus_messages_last_hour Messages this node took in during the last hour.")
	p("# TYPE aivoicebus_messages_last_hour gauge")
	p("aivoicebus_messages_last_hour %d", st.LastHour)
	p("# HELP aivoicebus_refused_total Sends refused since start, by reason: rate_sender, rate_machine, rate_pair (two agents replying in a loop), turns, deadline, loop (the same thing said over and over), unsigned.")
	p("# TYPE aivoicebus_refused_total counter")
	reasons := map[string]int64{"rate_sender": 0, "rate_machine": 0, "rate_pair": 0, "turns": 0, "deadline": 0, "loop": 0}
	for k, v := range st.RefusedBy {
		reasons[k] = v
	}
	reasons["unsigned"] = m.unsigned.Load()
	for _, k := range slices.Sorted(maps.Keys(reasons)) {
		p(`aivoicebus_refused_total{reason="%s"} %d`, label(k), reasons[k])
	}
	p("# HELP aivoicebus_decide_total Phone decisions (/v1/decide) since start, by outcome.")
	p("# TYPE aivoicebus_decide_total counter")
	decideOutcomes.Range(func(k, v any) bool {
		p(`aivoicebus_decide_total{outcome="%s"} %d`, label(k.(string)), v.(*atomic.Int64).Load())
		return true
	})
	p("# HELP aivoicebus_quarantined_records Records held only as a placeholder, because the original failed the replication check.")
	p("# TYPE aivoicebus_quarantined_records gauge")
	p("aivoicebus_quarantined_records %d", st.Quarantined)
	p("# HELP aivoicebus_refused_records_total Records dropped because their origin cannot be kept, since start.")
	p("# TYPE aivoicebus_refused_records_total counter")
	p("aivoicebus_refused_records_total %d", st.Refused)
	p("# HELP aivoicebus_retracted_messages Messages this node hides because their sender's machine retracted them.")
	p("# TYPE aivoicebus_retracted_messages gauge")
	p("aivoicebus_retracted_messages %d", st.Retracted)
	p("# HELP aivoicebus_retractions_ineffective Retractions held for a message by an identity not on its sender's machine, which change nothing.")
	p("# TYPE aivoicebus_retractions_ineffective gauge")
	p("aivoicebus_retractions_ineffective %d", st.RetractionsIneffective)
	p("# HELP aivoicebus_log_records Records held per origin node (the version vector).")
	p("# TYPE aivoicebus_log_records gauge")
	origins := make([]string, 0, len(h.Log))
	for o := range h.Log {
		origins = append(origins, o)
	}
	sort.Strings(origins)
	for _, o := range origins {
		p(`aivoicebus_log_records{origin="%s"} %d`, label(o), h.Log[o])
	}
	p("# HELP aivoicebus_requests_total Authenticated requests by token name.")
	p("# TYPE aivoicebus_requests_total counter")
	m.mu.Lock()
	names := make([]string, 0, len(m.requests))
	for n := range m.requests {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p(`aivoicebus_requests_total{token="%s"} %d`, label(n), m.requests[n].Load())
	}
	m.mu.Unlock()
	p("# HELP aivoicebus_unauthorized_total Requests refused for a missing or wrong token.")
	p("# TYPE aivoicebus_unauthorized_total counter")
	p("aivoicebus_unauthorized_total %d", m.unauthorized.Load())
	p("# HELP aivoicebus_secrets_refused_total Sends refused because the text looked like a credential.")
	p("# TYPE aivoicebus_secrets_refused_total counter")
	p("aivoicebus_secrets_refused_total %d", m.secrets.Load())
	p("# HELP aivoicebus_peer_up Whether the last pull from a peer succeeded.")
	p("# TYPE aivoicebus_peer_up gauge")
	p("# HELP aivoicebus_peer_lag_records Records a peer held that this node lacked at the last look.")
	p("# TYPE aivoicebus_peer_lag_records gauge")
	p("# HELP aivoicebus_peer_clock_skew_seconds A peer's clock minus this node's.")
	p("# TYPE aivoicebus_peer_clock_skew_seconds gauge")
	p("# HELP aivoicebus_peer_last_success_age_seconds Seconds since the last good pull from a peer (-1: never).")
	p("# TYPE aivoicebus_peer_last_success_age_seconds gauge")
	p("# HELP aivoicebus_peer_pulled_total Records taken from a peer since start.")
	p("# TYPE aivoicebus_peer_pulled_total counter")
	for _, pe := range h.Peers {
		up := 0
		if pe.OK {
			up = 1
		}
		age := -1.0
		if !pe.LastOK.IsZero() {
			age = time.Since(pe.LastOK).Seconds()
		}
		p(`aivoicebus_peer_up{peer="%s"} %d`, label(pe.URL), up)
		p(`aivoicebus_peer_lag_records{peer="%s"} %d`, label(pe.URL), pe.Lag)
		p(`aivoicebus_peer_clock_skew_seconds{peer="%s"} %.3f`, label(pe.URL), float64(pe.SkewMs)/1000)
		p(`aivoicebus_peer_last_success_age_seconds{peer="%s"} %.1f`, label(pe.URL), age)
		p(`aivoicebus_peer_pulled_total{peer="%s"} %d`, label(pe.URL), pe.Pulled)
	}
}
