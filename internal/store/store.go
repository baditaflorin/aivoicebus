// Package store is one node's durable, replicable state: an append-only log of
// messages and acks, with the inbox/thread/who views derived from it. It knows
// nothing about HTTP.
package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

type Store struct {
	mu   sync.Mutex
	dir  string
	node string
	log  map[string][]msg.Record    // origin -> records, index = seq-1
	msgs []msg.Message              // all known messages in arrival order
	acks map[string]map[string]bool // id -> by -> true
	// ackAt is when each reader first acknowledged a message and how it was shown to them, for receipts.
	ackAt map[string]map[string]msg.Receipt
	// gen counts applied records; changed is closed and replaced at each one, so a reader waiting
	// for news (a long-polled inbox) wakes as soon as anything arrives.
	gen     uint64
	changed chan struct{}
	leases  []int // indexes of lease messages (msg.KindLease), in arrival order
	nAck    int   // how many (id, by) receipts acks holds

	// retr holds the retractions the log carries (see msg/retract.go): message id -> who claimed it
	// -> the earliest time they did and whether any claim flagged a credential. Claims are kept
	// whether or not the message is held and whether or not their author may retract it: that is
	// decided when mail is read (retractionLocked), so the outcome does not depend on the order
	// records arrived in. They are not read receipts and are not in acks.
	retr map[string]*claims
	// Totals over the messages held that have claims, kept as claims and messages arrive so that
	// Stats (which every health read and every metrics scrape asks for, under the lock) costs
	// nothing however many claims a peer has sent.
	nRetracted int // messages with an authorised claim
	nInert     int // claims by an identity not on the sender's machine, one per (message, identity)

	// Retraction notices (see msg/retract.go): due lists, per reader, the retracted messages held
	// that it had read, kept as acks and retractions arrive in whatever order, so an inbox read looks
	// only at its own reader's list and not at every retraction. An entry is dropped once it can no
	// longer give a notice (the reader acked the notice, the window passed, ...) and noted again if
	// a later record could make it give one. countOnly holds the (message, reader) pairs whose every
	// receipt so far is a hook-count one: the session-start hook counted the message, an fyi, without
	// showing it, so the reader did not see the text.
	due       map[string]*dueList
	countOnly map[string]map[string]bool

	// forged holds the ids of messages held whose sender claims the machine the nodes speak as
	// (msg.IsSystemSender). A node never stores a message of its own, so these came from a peer
	// or an import: they are never delivered, and are listed with their body removed.
	forged map[string]bool

	// noticeIDs are the ids retraction notices have had (msg.NoticeID for each retracted message,
	// each probe tried): a node refuses to store a message sent with one, or sent as a reply to one
	// it does not hold, so a sender cannot plant a message a notice would then be taken for.
	noticeIDs map[string]bool
	// arrived is when this node first took in each claim dated ahead of its own clock, by
	// "origin/seq", kept in retract-arrived.json: the log holds only the claim's own time, and
	// replaying it at a restart would otherwise start the notice window again.
	arrived      map[string]time.Time
	lastDuePrune time.Time // when the due lists were last trimmed

	// Indexes into msgs, kept in step with it, so reading mail costs what it
	// returns and not the length of the whole history.
	idx      map[string]int          // message id -> index
	threads  map[string][]int        // thread id -> indices in arrival order
	byTo     map[string][]int        // the exact `to` -> indices in arrival order
	sels     map[string]msg.Selector // each distinct selector target, parsed (nil if malformed)
	answers  map[string][]answerRef  // question or task id -> the answers and claims to it
	lastSent map[string]time.Time    // sender -> time of its latest message
	seen     map[string]seenInfo     // identity -> last contact with THIS node
	now      func() time.Time        // the clock; replaced in tests

	lastPrune time.Time // when seen was last trimmed

	presenceDirty bool      // seen changed since it was last written to presence.json
	presenceSaved time.Time // when it was last written, by the wall clock: the throttle is about disk writes, not message time

	dups, limited int64 // messages dropped as repeats, and sends refused by the rate limit, since start

	quarantined int       // placeholders held in the log, each standing for a record that failed msg.Record.ValidateReplicated
	refused     int64     // records dropped because their origin cannot be kept, since start
	logw        io.Writer // one line per quarantined or refused record; nil says nothing

	sends     map[string][]time.Time  // sender -> when it sent recently, oldest first
	recent    map[[32]byte]recentSend // fingerprint of a recent message -> its id
	sendLimit int                     // messages a sender may send per sendWindow

	retracts     map[string][]time.Time // identity -> when its retractions were written recently, oldest first
	retractLimit int                    // retractions an identity may write per sendWindow
}

// retractionRec is what one identity's retraction claims on one message, merged over every record
// it wrote: the earliest time, and whether any of them was a retract-leak. due is the earliest time
// clamped to when this node took the record in: a notice is owed from then, so a claim dated far in
// the future (a wrong clock, or a peer that wants a notice to linger) does not keep one owed.
type retractionRec struct {
	at   time.Time
	due  time.Time
	leak bool
}

// retraction is a message's effective retraction: that of the earliest authorised claim.
type retraction struct {
	by   string
	at   time.Time
	leak bool // any authorised claim flagged a credential
}

// fold takes in one authorised claim: the earliest (then the lowest identity) is the retractor, and
// any claim flagging a credential flags it. first says r holds no claim yet.
func (r *retraction) fold(first bool, by string, at time.Time, leak bool) {
	if first {
		*r = retraction{by: by, at: at, leak: leak}
		return
	}
	r.leak = r.leak || leak
	if at.Before(r.at) || (at.Equal(r.at) && by < r.by) {
		r.by, r.at = by, at
	}
}

// claims is every retraction claim on one message, and what they come to for the sender the message
// is held under, kept so that reading mail does not look at every claim: it is worked out when the
// message and a claim on it are both held (valid, see settleLocked), and a claim that arrives later
// is folded into it. Claims on a message not held yet are only kept.
type claims struct {
	by    map[string]retractionRec
	from  string
	eff   retraction
	has   bool // an authorised claim exists
	inert int  // identities claiming without authority, as counted in Store.nInert
	valid bool // eff, has and inert are worked out for from, and the totals include them

	due time.Time // when notices about it are owed from: the earliest due of the authorised claims

	// The id the notice about this message has: NoticeID(id, probe) for the first probe no held
	// message has. Messages are never removed, so a probe found taken stays taken and the search
	// goes on from where it stopped.
	probe int
	nid   string
}

// dueList is one reader's retracted messages it may be owed a notice for: their ids, in order of
// their retraction time once sorted (an earlier claim can arrive later, which unsorts it).
type dueList struct {
	ids    map[string]bool
	order  []string
	sorted bool
}

// answerRef is an answer to a question or a claim on a task: its id (so a retraction of it can be
// found), its sender, when the node stamped it (the earliest claim takes a task) and whether it is
// a claim (a claim takes a task but answers no question).
type answerRef struct {
	id, from string
	at       time.Time
	claim    bool
}

// recentSend is a message a node accepted a moment ago, kept to recognise an
// identical one sent again.
type recentSend struct {
	id string
	at time.Time
}

// Bounds on what a node remembers about who has been in contact, so a roster
// cannot grow without limit (someone holding the token could invent identities).
const (
	seenKeep = 7 * 24 * time.Hour // a session not heard from for this long leaves the roster
	maxSeen  = 5000               // hard cap on remembered sessions; the oldest go first
)

// Guards against an agent stuck in a loop. They are courtesies, not security (the
// shared token lets anyone claim any sender), they live in memory, and a node
// restart forgets them.
const (
	defaultSendLimit    = 20               // messages one sender may send per sendWindow
	defaultRetractLimit = 60               // retractions one identity may write per sendWindow
	sendWindow          = time.Hour        // the span the limit is counted over
	dupWindow           = 10 * time.Minute // an identical message within this span is the same message
	maxSenders          = 2000             // senders remembered for rate limiting
	maxRecent           = 1024             // fingerprints remembered for duplicate detection
)

// RateLimitError says a sender has used up its messages for now.
type RateLimitError struct {
	Limit      int
	RetryAfter time.Duration
	What       string // what is counted, when it is not messages ("retractions")
}

func (e *RateLimitError) Error() string {
	wait := e.RetryAfter.Round(time.Minute)
	if wait < time.Minute {
		wait = time.Minute
	}
	if e.What != "" {
		return fmt.Sprintf("rate limit: %d %s an hour per sender is the most, and this sender has used them. Try again in %s.", e.Limit, e.What, wait)
	}
	return fmt.Sprintf("rate limit: %d messages an hour per sender is the most, and this sender has used them. Try again in %s. Send fewer, shorter messages, or ask the user.", e.Limit, wait)
}

// seenInfo is what this node last heard from one identity.
type seenInfo struct {
	at     time.Time
	labels msg.Labels
	title  string // what the session's tool last called it (msg.CleanTitle); "" when it declared none
	// waitingUntil is when the inbox read the session is waiting in (a long poll) ends: until then
	// it is listening right now. Kept on this node only.
	waitingUntil time.Time
}

// Option customises Open.
type Option func(*Store)

// WithLog makes the store write one line to w for each record it quarantines or refuses, naming
// the origin and number and why, never the content.
func WithLog(w io.Writer) Option { return func(s *Store) { s.logw = w } }

// Open loads (or creates) the store in dir for the given node id.
func Open(dir, node string, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, node: node, log: map[string][]msg.Record{},
		idx: map[string]int{}, threads: map[string][]int{}, byTo: map[string][]int{}, sels: map[string]msg.Selector{}, answers: map[string][]answerRef{}, lastSent: map[string]time.Time{},
		acks: map[string]map[string]bool{}, ackAt: map[string]map[string]msg.Receipt{}, changed: make(chan struct{}), retr: map[string]*claims{}, due: map[string]*dueList{}, countOnly: map[string]map[string]bool{}, forged: map[string]bool{}, noticeIDs: map[string]bool{}, arrived: map[string]time.Time{}, seen: map[string]seenInfo{}, now: time.Now,
		sends: map[string][]time.Time{}, recent: map[[32]byte]recentSend{}, sendLimit: defaultSendLimit,
		retracts: map[string][]time.Time{}, retractLimit: defaultRetractLimit}
	for _, o := range opts {
		o(s)
	}
	s.loadArrived()
	err := readLines(filepath.Join(dir, "log.jsonl"), func(b []byte, over bool) {
		var r msg.Record
		if over { // too long to parse: only its place in the numbering is kept
			var ok bool
			if r, ok = headRecord(b); !ok {
				return
			}
		} else if json.Unmarshal(b, &r) != nil {
			return
		}
		if r, ok := s.vetLocked(r); ok {
			s.applyLocked(r)
		}
	})
	if terr := repairTail(filepath.Join(dir, "log.jsonl")); terr != nil && err == nil {
		err = terr
	}
	s.loadPresence()
	return s, err
}

// repairTail makes sure the log ends with a newline. A crash or power loss in the middle of an
// append leaves a last line with no end; the next record appended would be glued onto it and
// both lost to the parser, silently stalling every later record of that origin. The cut line was
// never acknowledged to a client, so it is set aside, not kept.
func repairTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return err
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, fi.Size()-1); err != nil || last[0] == '\n' {
		return err
	}
	// find the start of the cut line (the byte after the last newline)
	const chunk = 64 << 10
	end := fi.Size()
	cut := int64(0)
	for end > 0 {
		start := max(end-chunk, 0)
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return err
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			cut = start + int64(i) + 1
			break
		}
		end = start
	}
	tail := make([]byte, fi.Size()-cut)
	if _, err := f.ReadAt(tail, cut); err != nil {
		return err
	}
	var r msg.Record
	if len(tail) <= maxLine && json.Unmarshal(tail, &r) == nil && r.Origin != "" { // a complete record that only lacks its newline
		_, err = f.WriteAt([]byte{'\n'}, fi.Size())
		return err
	}
	_ = os.WriteFile(path+".torn", tail, 0o600) // keep what was cut, for whoever wants to look
	return f.Truncate(cut)
}

// SetSendLimit changes how many messages one sender may send per hour.
func (s *Store) SetSendLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendLimit = n
}

// SetClock replaces the store's clock, so tests can move time.
func (s *Store) SetClock(now func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// Dir is the data directory the store was opened on.
func (s *Store) Dir() string { return s.dir }

// Longest log line replay parses whole (a record that passes msg.Record.ValidateReplicated is
// under 250 KB even with every character escaped), and how much of a longer one it looks at.
const (
	maxLine = 4 << 20
	headLen = 4 << 10
)

// readLines calls fn with each line of the file, newline included. A line of any length is read
// (a peer's record once got in at 1.5 MB, and a scanner with a limit then stopped the node from
// starting); one longer than maxLine is given as its first headLen bytes, with over set.
func readLines(path string, fn func(line []byte, over bool)) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	var line []byte
	over, ended := false, false
	for {
		chunk, rerr := br.ReadSlice('\n')
		ended = len(chunk) > 0 && chunk[len(chunk)-1] == '\n'
		if !over {
			line = append(line, chunk...)
			if len(line) > maxLine {
				over, line = true, line[:headLen]
			}
		}
		if rerr == bufio.ErrBufferFull { // the line goes on
			continue
		}
		// A line too long to parse that never ended is the torn tail of a crashed append, which
		// repairTail cuts: it was never acknowledged, so it keeps no place in the numbering.
		if len(line) > 0 && (ended || !over) {
			fn(line, over)
		}
		line, over = line[:0], false
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// headRecord reads the origin and number from the start of a record line too long to parse,
// which is all that keeps that origin's numbering contiguous. json.Marshal writes both first;
// a line that does not begin that way is given up.
func headRecord(b []byte) (msg.Record, bool) {
	var r msg.Record
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return r, false
	}
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return r, false
		}
		switch k {
		case "origin":
			err = dec.Decode(&r.Origin)
		case "seq":
			err = dec.Decode(&r.Seq)
		default:
			return r, r.Origin != "" && r.Seq > 0
		}
		if err != nil {
			return r, false
		}
	}
	return r, r.Origin != "" && r.Seq > 0
}

func appendLine(path string, v any) error {
	b, _ := json.Marshal(v)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

// applyLocked applies r if it is the next record from its origin; it is
// idempotent, so duplicate or replayed pulls are harmless.
func (s *Store) applyLocked(r msg.Record) bool {
	if r.Origin == "" || r.Seq != uint64(len(s.log[r.Origin]))+1 {
		return false
	}
	s.log[r.Origin] = append(s.log[r.Origin], r)
	s.gen++
	close(s.changed)
	s.changed = make(chan struct{})
	if r.Msg != nil {
		s.addMsgLocked(*r.Msg)
	}
	if r.Ack != nil {
		if mid, leak, ok := msg.RetractedID(r.Ack.ID); ok {
			s.addRetractionLocked(mid, r.Ack.By, r.Ack.At, leak, r.Origin+"/"+strconv.FormatUint(r.Seq, 10)) // its Via and Chars are not read
		} else {
			s.addAck(*r.Ack)
		}
		if r.Ack.ID == QuarantineID && r.Ack.By == QuarantineBy {
			s.quarantined++
		}
	}
	return true
}

// What stands in the log for a record that failed msg.Record.ValidateReplicated: a well-formed
// acknowledgement of nothing, so every consumer of the feed (the dashboard drops a record that holds
// neither a message nor an ack, and then stops following that origin) can read it, and it
// holds the record's place in the numbering. No message has this id (those are 10 hex characters).
const (
	QuarantineID = "quarantined"
	QuarantineBy = "aivoicebus/quarantine"
)

// quarantine is the placeholder at (origin, seq). It is the same on every node, and carries
// nothing of the record it replaces, so nodes that quarantine the same record hold the same one.
func quarantine(origin string, seq uint64) msg.Record {
	return msg.Record{Origin: origin, Seq: seq, Ack: &msg.Ack{ID: QuarantineID, By: QuarantineBy}}
}

// vetLocked decides what to store for r, a record from a peer or read back from the log, if it is
// the next one of its origin (ok is false otherwise, and for an origin that cannot be kept at
// all). A record that fails msg.Record.ValidateReplicated comes back as its placeholder: leaving
// the number out would stall that origin for good, since every later record would look like a gap.
func (s *Store) vetLocked(r msg.Record) (msg.Record, bool) {
	if r.Seq != uint64(len(s.log[r.Origin]))+1 {
		return r, false
	}
	err := r.ValidateReplicated()
	switch {
	case err == nil:
		if r.Msg != nil && (r.Msg.Retracted != nil || r.Msg.Verified != "" || r.Msg.Private) { // response only: what a release that predates retraction does when it decodes and writes the record again; and what only a reading client may say
			m := *r.Msg
			m.Retracted = nil
			m.Verified, m.Private = "", false
			r.Msg = &m
		}
		return r, true
	case errors.Is(err, msg.ErrReplicatedOrigin):
		if s.refused++; s.refused%1000 == 1 { // a peer that keeps offering it is counted, but not logged on every pull
			s.logf("refused a record whose origin cannot be kept (%d so far)", s.refused)
		}
		return r, false
	}
	s.logf("quarantined record %d of origin %q: %v", r.Seq, r.Origin, err)
	return quarantine(r.Origin, r.Seq), true
}

func (s *Store) logf(format string, args ...any) {
	if s.logw != nil {
		fmt.Fprintf(s.logw, format+"\n", args...)
	}
}

// addMsgLocked appends m unless a message with its id is already held (the same
// message written to two nodes) and updates the indexes.
func (s *Store) addMsgLocked(m msg.Message) {
	if _, dup := s.idx[m.ID]; dup {
		return
	}
	i := len(s.msgs)
	s.msgs = append(s.msgs, m)
	s.idx[m.ID] = i
	s.threads[m.Thread] = append(s.threads[m.Thread], i)
	s.byTo[m.To] = append(s.byTo[m.To], i)
	if m.Kind == msg.KindLease {
		s.leases = append(s.leases, i)
	}
	if msg.IsSelector(m.To) {
		if _, known := s.sels[m.To]; !known {
			sel, err := msg.ParseSelector(m.To)
			if err != nil {
				sel = nil
			}
			s.sels[m.To] = sel
		}
	}
	forged := msg.IsSystemSender(m.From)
	if forged {
		s.forged[m.ID] = true
	}
	if !forged && msg.Takes(m) { // a forged answer or claim closes nothing
		s.answers[m.Re] = append(s.answers[m.Re], answerRef{id: m.ID, from: m.From, at: m.At, claim: m.Kind == msg.KindClaim})
	}
	if m.At.After(s.lastSent[m.From]) {
		s.lastSent[m.From] = m.At
	}
	if c := s.retr[m.ID]; c != nil { // claims that arrived first can now be told apart
		s.settleLocked(m.ID, c, m.From)
	}
}

// closedForLocked reports whether m need not be put in front of me: a question to several
// sessions that someone other than its sender has answered, or a task to several sessions that a
// session other than me took first (see msg.TaskOwner). An answer or claim its sender retracted
// does not count, so retracting it opens the question or the task again.
func (s *Store) closedForLocked(m msg.Message, me string) bool {
	if !msg.IsBulletin(m.To) {
		return false
	}
	switch m.Kind {
	case msg.KindQuestion:
		for _, a := range s.answers[m.ID] {
			if !a.claim && a.from != m.From && !s.retractedLocked(a.id, a.from) {
				return true
			}
		}
	case msg.KindTask:
		owner, taken := s.taskOwnerLocked(m)
		return taken && owner.From != me
	}
	return false
}

// taskOwnerLocked is the session that holds task m, if one has taken it.
func (s *Store) taskOwnerLocked(m msg.Message) (msg.TakeRef, bool) {
	var takes []msg.TakeRef
	for _, a := range s.answers[m.ID] {
		if !s.retractedLocked(a.id, a.from) {
			takes = append(takes, msg.TakeRef{ID: a.id, From: a.from, At: a.at})
		}
	}
	return msg.TaskOwner(m.From, takes)
}

// TaskOwner is the session that holds the task with this id, and whether anyone has taken it.
func (s *Store) TaskOwner(id string) (msg.TakeRef, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, ok := s.idx[id]
	if !ok || s.msgs[i].Kind != msg.KindTask {
		return msg.TakeRef{}, false
	}
	return s.taskOwnerLocked(s.msgs[i])
}

// addRetractionLocked notes that by claims to retract message mid, as of at. A second claim by the
// same identity keeps the earlier time and the stronger flag, so claims merge in any order.
func (s *Store) addRetractionLocked(mid, by string, at time.Time, leak bool, key string) {
	c := s.retr[mid]
	if c == nil {
		c = &claims{by: map[string]retractionRec{}}
		s.retr[mid] = c
		if i, held := s.idx[mid]; held {
			s.settleLocked(mid, c, s.msgs[i].From)
		}
	}
	cur, known := c.by[by]
	if !known || at.Before(cur.at) {
		cur.at = at
	}
	due := s.claimDueLocked(key, at)
	if !known || due.Before(cur.due) {
		cur.due = due
	}
	cur.leak = cur.leak || leak
	c.by[by] = cur
	if !c.valid {
		return
	}
	if msg.MayRetract(by, c.from) {
		was, wasDue := c.eff, c.due
		c.eff.fold(!c.has, by, at, leak)
		if !c.has || due.Before(c.due) {
			c.due = due
		}
		switch {
		case !c.has:
			c.has = true
			s.nRetracted++
			s.noteReadersLocked(mid, c)
		case !c.due.Equal(wasDue) || c.eff.leak != was.leak:
			// the readers' lists are ordered by these, and a notice not owed for an fyi is owed for a leak
			s.unsortReadersLocked(mid)
			s.noteReadersLocked(mid, c)
		}
	} else if !known {
		c.inert++
		s.nInert++
	}
}

// maxArrived bounds retract-arrived.json: claims dated ahead past it are owed from when the log is
// next replayed, which is the most a peer sending that many gains.
const maxArrived = 10000

// claimDueLocked is the time a notice about a claim is owed from: the claim's own time, but no later
// than when this node first took it in, so a claim dated ahead (a wrong clock, or a peer that wants a
// notice to linger) does not keep one owed. Taking it in is remembered on disk for a claim dated
// ahead (key is "origin/seq"; "" is not remembered), so replaying the log at a restart gives the same
// answer and the window is not started again. A claim not dated ahead is just its own time.
func (s *Store) claimDueLocked(key string, at time.Time) time.Time {
	if first, ok := s.arrived[key]; ok {
		return minTime(at, first)
	}
	now := s.now().UTC()
	if !at.After(now) {
		return at
	}
	if key != "" && len(s.arrived) < maxArrived {
		s.arrived[key] = now
		s.saveArrived()
	}
	return now
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

const arrivedFile = "retract-arrived.json"

// loadArrived reads retract-arrived.json; a missing or damaged file is an empty one (claims dated
// ahead are then owed from the time of the replay, as in a release that kept no file).
func (s *Store) loadArrived() {
	b, err := os.ReadFile(filepath.Join(s.dir, arrivedFile))
	if err != nil {
		return
	}
	var m map[string]time.Time
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for k, v := range m {
		if len(s.arrived) < maxArrived {
			s.arrived[k] = v
		}
	}
}

// saveArrived writes retract-arrived.json, best effort: it happens only when a claim dated ahead is
// first taken in, which an honest mesh does not do.
func (s *Store) saveArrived() {
	b, err := json.Marshal(s.arrived)
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, arrivedFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, filepath.Join(s.dir, arrivedFile))
	}
}

// settleLocked works out c for a message sent as from, from every claim held, and puts what it
// comes to into the store's totals in place of what they held of c before. It runs once for a
// message, when the message and a claim are both held, and then claims are folded in one at a time.
func (s *Store) settleLocked(mid string, c *claims, from string) {
	if c.valid {
		if c.has {
			s.nRetracted--
		}
		s.nInert -= c.inert
	}
	c.eff, c.has, c.inert, c.due = retraction{}, false, 0, time.Time{}
	for by, r := range c.by {
		if msg.MayRetract(by, from) {
			if !c.has || r.due.Before(c.due) {
				c.due = r.due
			}
			c.eff.fold(!c.has, by, r.at, r.leak)
			c.has = true
		} else {
			c.inert++
		}
	}
	c.from, c.valid = from, true
	if c.has {
		s.nRetracted++
		s.noteReadersLocked(mid, c)
	}
	s.nInert += c.inert
}

// retractionLocked is the effective retraction of the message with this id and sender, if any: the
// earliest claim (then the lowest identity) among those whose author is on the sender's machine.
// It looks only at the claims on that one message, so reading mail costs nothing while there are none.
func (s *Store) retractionLocked(id, from string) (retraction, bool) {
	c := s.retr[id]
	if c == nil {
		return retraction{}, false
	}
	if !c.valid || c.from != from { // not so for a message that is held: it was settled when both arrived
		s.settleLocked(id, c, from)
	}
	return c.eff, c.has
}

func (s *Store) retractedLocked(id, from string) bool {
	_, ok := s.retractionLocked(id, from)
	return ok
}

// viewLocked is message i as a reader is shown it: itself, or when it was retracted a copy
// whose body is the tombstone. The stored message is never changed.
func (s *Store) viewLocked(i int) msg.Message {
	m := s.msgs[i]
	if len(s.retr) == 0 && len(s.forged) == 0 {
		return m
	}
	return s.tombstoneLocked(m)
}

// tombstoneLocked is m, or if m was retracted a copy that tells who withdrew it and says nothing
// of what it said.
func (s *Store) tombstoneLocked(m msg.Message) msg.Message {
	if s.forged[m.ID] {
		return forgedView(m)
	}
	if r, ok := s.retractionLocked(m.ID, m.From); ok {
		return tombstone(m, r)
	}
	return m
}

// tombstone is the copy of m shown in its place once it is retracted: nothing of the body is kept,
// and the other fields are as the sender wrote them.
func tombstone(m msg.Message, r retraction) msg.Message {
	m.Body = msg.TombstoneBody(r.by, r.at, r.leak)
	m.FromTitle, m.FromModel = "", "" // a withdrawn message keeps nothing of its sender's session
	m.Retracted = &msg.Retraction{By: r.by, At: r.at, Leak: r.leak}
	return m
}

// ForgedBody is what stands in for the body of a stored message whose sender claims the machine the
// nodes speak as: what a node says of its own (a retraction notice) is never stored, so it is forged.
const ForgedBody = "[forged system message removed]"

// forgedView is the copy of a forged message listed by --all and threads.
func forgedView(m msg.Message) msg.Message {
	m.Body = ForgedBody
	m.FromTitle, m.FromModel = "", ""
	return m
}

// commitLocked appends a locally originated record durably, then applies it.
func (s *Store) commitLocked(r msg.Record) error {
	r.Origin = s.node
	r.Seq = uint64(len(s.log[s.node])) + 1
	if err := appendLine(filepath.Join(s.dir, "log.jsonl"), r); err != nil {
		return err
	}
	s.applyLocked(r)
	return nil
}

// VV returns the version vector.
func (s *Store) VV() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	vv := map[string]uint64{}
	for o, l := range s.log {
		vv[o] = uint64(len(l))
	}
	return vv
}

// Since returns records the caller (with version vector vv) is missing.
func (s *Store) Since(vv map[string]uint64, limit int) []msg.Record {
	return s.SinceBytes(vv, limit, 0)
}

// SinceBytes is Since that also stops when the records would encode to more than maxBytes
// (0 means no such limit), so a page costs about that much however large the records are. It
// always returns at least one record when any is missing, so a caller that asks again until it
// gets nothing still gets through a record larger than the limit.
func (s *Store) SinceBytes(vv map[string]uint64, limit, maxBytes int) []msg.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []msg.Record
	size := 0
	for _, o := range slices.Sorted(maps.Keys(s.log)) {
		l := s.log[o]
		for i := vv[o]; i < uint64(len(l)) && len(out) < limit; i++ {
			if maxBytes > 0 {
				b, _ := json.Marshal(l[i])
				if len(out) > 0 && size+len(b)+1 > maxBytes {
					return out
				}
				size += len(b) + 1
			}
			out = append(out, l[i])
		}
	}
	return out
}

// Merge applies records pulled from a peer, persisting those that were new. A record that fails
// msg.Record.ValidateReplicated is stored as its placeholder instead (see vetLocked), and what
// is persisted, and passed on to other nodes, is the placeholder.
func (s *Store) Merge(recs []msg.Record) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range recs {
		r, ok := s.vetLocked(r)
		if ok && s.applyLocked(r) {
			if err := appendLine(filepath.Join(s.dir, "log.jsonl"), r); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}

func (s *Store) addAck(a msg.Ack) {
	if s.acks[a.ID] == nil {
		s.acks[a.ID] = map[string]bool{}
	}
	first := !s.acks[a.ID][a.By]
	if first {
		s.nAck++
	}
	s.acks[a.ID][a.By] = true
	if s.ackAt[a.ID] == nil {
		s.ackAt[a.ID] = map[string]msg.Receipt{}
	}
	if cur, ok := s.ackAt[a.ID][a.By]; !ok || a.At.Before(cur.At) || (cur.Via == msg.ViaHookCount && a.Via != msg.ViaHookCount) {
		// the earliest read, except that being read in full counts over being only counted
		at := a.At
		if ok && cur.Via != msg.ViaHookCount && cur.At.Before(at) {
			at = cur.At
		}
		s.ackAt[a.ID][a.By] = msg.Receipt{By: a.By, At: at, Via: a.Via}
	}
	switch {
	case a.Via == msg.ViaHookCount && first:
		if s.countOnly[a.ID] == nil {
			s.countOnly[a.ID] = map[string]bool{}
		}
		s.countOnly[a.ID][a.By] = true
	case a.Via != msg.ViaHookCount && s.countOnly[a.ID][a.By]: // read in full after all
		delete(s.countOnly[a.ID], a.By)
		if len(s.countOnly[a.ID]) == 0 {
			delete(s.countOnly, a.ID)
		}
	}
	if c := s.retr[a.ID]; c != nil && c.valid && c.has { // read a message that is retracted: it may be owed a notice
		s.noteDueLocked(a.By, a.ID)
	}
}

// Send stores a message. A message id already applied is returned as-is, so a
// client that wrote to several nodes causes no duplicate; so is an identical
// message (same sender, target, reply and text) sent again within dupWindow. A
// sender over its hourly limit gets a *RateLimitError.
func (s *Store) Send(m msg.Message) (msg.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.Retracted = nil                 // what a node says about a message, not something a sender sets
	m.Verified, m.Private = "", false // what a reading client finds, never stored
	if !msg.ValidID(m.ID) {
		m.ID = msg.NewID()
	} else if p, ok := s.byIDLocked(m.ID); ok { // client wrote to several nodes: idempotent
		return s.tombstoneLocked(p), nil // a message that was retracted does not come back with its text
	} else if s.noticeIDs[m.ID] {
		return msg.Message{}, ErrReservedID
	}
	if _, held := s.idx[m.Re]; m.Re != "" && !held && s.noticeIDs[m.Re] {
		return msg.Message{}, ErrReplyToNotice
	}
	now := s.now().UTC()
	// The same words sent again with another priority, kind or lifetime are not the same message:
	// an fyi repeated as urgent, or a note repeated as the answer to a question, must be stored.
	key := sha256.Sum256([]byte(m.From + "\x00" + m.To + "\x00" + m.Re + "\x00" + m.Prio + "\x00" + m.Kind + "\x00" + strconv.FormatInt(m.TTL, 10) + "\x00" + m.Body))
	if d, ok := s.recent[key]; ok && now.Sub(d.at) < dupWindow {
		// A retracted message is not "the same message": sending the words again after undoing
		// them is a new message, or undo-send would make the text impossible to send.
		if p, ok := s.byIDLocked(d.id); ok && !s.retractedLocked(p.ID, p.From) {
			s.dups++
			return p, nil
		}
	}
	if err := s.allowSendLocked(m.From, now); err != nil {
		s.limited++
		return msg.Message{}, err
	}
	m.At = now
	if m.TTL > 0 { // the node's clock decides when a message expires, not the sender's
		exp := m.At.Add(time.Duration(m.TTL) * time.Second)
		m.Exp, m.TTL = &exp, 0
	}
	if m.Thread == "" {
		m.Thread = m.ID
		if m.Re != "" {
			if p, ok := s.byIDLocked(m.Re); ok {
				m.Thread = p.Thread
			}
		}
	}
	if err := s.commitLocked(msg.Record{Msg: &m}); err != nil {
		return m, err
	}
	s.rememberLocked(key, m.ID, now)
	return m, nil
}

func (s *Store) byIDLocked(id string) (msg.Message, bool) {
	if i, ok := s.idx[id]; ok {
		return s.msgs[i], true
	}
	return msg.Message{}, false
}

// allowSendLocked counts one send by from at now, or refuses it if from already
// sent sendLimit messages within sendWindow.
func (s *Store) allowSendLocked(from string, now time.Time) error {
	return allowLocked(s.sends, from, s.sendLimit, "", now)
}

// allowLocked counts one use by who in used at now, or refuses it if who already used limit
// within sendWindow. what names what is counted when it is not messages.
func allowLocked(used map[string][]time.Time, who string, limit int, what string, now time.Time) error {
	ts := used[who]
	for len(ts) > 0 && !ts[0].After(now.Add(-sendWindow)) {
		ts = ts[1:]
	}
	if len(ts) >= limit {
		used[who] = ts
		return &RateLimitError{Limit: limit, RetryAfter: ts[0].Add(sendWindow).Sub(now), What: what}
	}
	used[who] = append(ts, now)
	if len(used) > maxSenders {
		for k, v := range used { // senders quiet for the whole window owe nothing
			if len(v) == 0 || !v[len(v)-1].After(now.Add(-sendWindow)) {
				delete(used, k)
			}
		}
		if len(used) > maxSenders { // invented senders: forget the quietest
			keys := slices.SortedFunc(maps.Keys(used), func(a, b string) int {
				return used[a][len(used[a])-1].Compare(used[b][len(used[b])-1])
			})
			for _, k := range keys[:len(keys)-maxSenders*9/10] {
				delete(used, k)
			}
		}
	}
	return nil
}

// rememberLocked notes that the message with fingerprint key was accepted at now.
func (s *Store) rememberLocked(key [32]byte, id string, now time.Time) {
	s.recent[key] = recentSend{id: id, at: now}
	if len(s.recent) <= maxRecent {
		return
	}
	for k, v := range s.recent {
		if now.Sub(v.at) >= dupWindow {
			delete(s.recent, k)
		}
	}
	if len(s.recent) > 2*maxRecent { // duplicate detection is best effort: start over
		s.recent = map[[32]byte]recentSend{}
	}
}

// Ack records that identity `by` has read message id.
func (s *Store) Ack(id, by string) error { return s.AckWith(msg.Ack{ID: id, By: by}) }

// AckWith records a. The time is the node's; a's Via and Chars (the delivery
// receipt) are kept as given.
func (s *Store) AckWith(a msg.Ack) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.At = s.now().UTC()
	return s.commitLocked(msg.Record{Ack: &a})
}

// ErrReservedID is what Send says of a message whose chosen id is one a retraction notice has: such
// an id is for the notice, and a message holding it would be taken for it.
var ErrReservedID = errors.New("that message id is reserved for a retraction notice; leave the id out")

// ErrReplyToNotice is what Send says of a reply to a retraction notice, which is made up by nodes
// and is no message to reply to.
var ErrReplyToNotice = errors.New("that id is a retraction notice, not a message: there is nothing to reply to")

// ErrUnknown is what Retract says of a message this node does not hold. A node never records a
// retraction for a message it has not got: it cannot tell a message still on its way from a typo,
// and the client asks another node.
var ErrUnknown = errors.New("this node does not hold that message (it may not have reached it yet, or the id is wrong)")

// ErrBadRetract is what Retract says of an id that names nothing retractable, or an identity that
// cannot claim to be anyone.
var ErrBadRetract = errors.New("not a message id (ten lowercase hex digits) and an identity")

// ErrNotSender is what Retract says when the identity is not on the machine that sent the message.
type ErrNotSender struct{ Sender string }

func (e *ErrNotSender) Error() string {
	return fmt.Sprintf("only a session on %s, the machine that sent this message, can retract it", msg.HostOf(e.Sender))
}

// Retract records that by retracts message id, unless it already is retracted at least as strongly
// (leak: the sender says the text held a credential), and says what came of it. The record is an
// ordinary ack (see msg/retract.go), committed like any other, and takes effect on this node at
// once. It is refused for a message this node does not hold (ErrUnknown) and for an identity not on
// the machine that sent it (*ErrNotSender), and counts against the identity's hourly allowance of
// retractions only when it writes something, so refusals and repeats cost nothing.
func (s *Store) Retract(id, by string, leak bool) (msg.RetractResult, error) {
	return s.retract(id, by, leak, false)
}

// RetractDryRun is Retract that writes nothing and counts against nothing: the result says what
// Retract would do (Would) and how many other sessions had read the message.
func (s *Store) RetractDryRun(id, by string, leak bool) (msg.RetractResult, error) {
	return s.retract(id, by, leak, true)
}

func (s *Store) retract(id, by string, leak, dry bool) (msg.RetractResult, error) {
	if !msg.ValidRetractID(id) || !msg.ValidIdentity(by) || by == QuarantineBy {
		return msg.RetractResult{}, ErrBadRetract
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byIDLocked(id)
	if !ok {
		return msg.RetractResult{}, ErrUnknown
	}
	if !msg.MayRetract(by, m.From) {
		return msg.RetractResult{}, &ErrNotSender{Sender: m.From}
	}
	now := s.now().UTC()
	state := msg.RetractRetracted
	if cur, retracted := s.retractionLocked(id, m.From); retracted {
		state = msg.RetractAlready
		if leak && !cur.leak {
			state = msg.RetractEscalated
		}
	}
	res := msg.RetractResult{OK: true, State: state, Sender: m.From, ReadBy: s.readByLocked(m, by), Expired: m.Expired(now)}
	if dry {
		res.State, res.Would = msg.RetractDryRun, state
		return res, nil
	}
	if state == msg.RetractAlready {
		return res, nil
	}
	if err := allowLocked(s.retracts, by, s.retractLimit, "retractions", now); err != nil {
		return msg.RetractResult{}, err
	}
	ackID := msg.RetractPrefix + id
	if leak {
		ackID = msg.RetractLeakPrefix + id
	}
	if err := s.commitLocked(msg.Record{Ack: &msg.Ack{ID: ackID, By: by, At: now}}); err != nil {
		return msg.RetractResult{}, err
	}
	return res, nil
}

// readByLocked counts the identities this node holds a read receipt from for m, other than its
// sender, the one retracting it and the quarantine placeholder: the sessions that may have acted
// on it. Receipts written to other nodes may not have arrived, so it is a lower bound.
func (s *Store) readByLocked(m msg.Message, retractor string) int {
	n := 0
	for by := range s.acks[m.ID] {
		if by != m.From && by != retractor && by != QuarantineBy {
			n++
		}
	}
	return n
}

// SetRetractLimit changes how many retractions one identity may write per hour.
func (s *Store) SetRetractLimit(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retractLimit = n
}

// Inbox returns the messages addressed to me (unread only unless all) for a
// session that declares no labels, and notes that me was just in contact.
func (s *Store) Inbox(me string, all bool) []msg.Message { return s.InboxFor(me, all, nil) }

// InboxFor is Inbox for a session that declares labels: selector messages
// ("@repo=x") reach it when its labels satisfy them. The declared labels are
// remembered for the roster, replacing whatever it declared before.
func (s *Store) InboxFor(me string, all bool, labels msg.Labels) []msg.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	s.seen[me] = seenInfo{at: now, labels: labels, title: s.seen[me].title, waitingUntil: s.seen[me].waitingUntil} // a read that declares no title keeps the last one
	s.pruneSeenLocked(now)
	s.presenceDirty = true
	s.flushPresenceLocked(now, false)
	var out []msg.Message
	for _, i := range s.candidatesLocked(me, labels) {
		m := s.msgs[i]
		if m.From == me || !msg.MatchesLabeled(m.To, me, labels) { // the index is a superset filter; the rule is the authority
			continue
		}
		if len(s.forged) > 0 && s.forged[m.ID] { // never delivered; --all lists it without its words
			if all {
				out = append(out, forgedView(m))
			}
			continue
		}
		var ret retraction
		retracted := false
		if len(s.retr) > 0 {
			ret, retracted = s.retractionLocked(m.ID, m.From)
		}
		if !all && retracted { // withdrawn: not delivered, however urgent; --all lists it as a tombstone
			continue
		}
		if !all && m.Expired(now) { // stale news is never delivered; --all still lists it
			continue
		}
		if !all && (s.acks[m.ID][me] || s.closedForLocked(m, me)) {
			continue
		}
		if retracted {
			m = tombstone(m, ret)
		}
		out = append(out, m)
	}
	if !all { // first: a reader that lists only the first few must not miss them
		out = append(s.noticesLocked(me, now), out...)
	}
	return out
}

// candidatesLocked lists, in arrival order, the messages whose target could reach
// me: every `to` form that msg.MatchesLabeled would accept for this identity, plus
// each selector target whose terms the labels satisfy. It looks only at those lists.
func (s *Store) candidatesLocked(me string, labels msg.Labels) []int {
	mh, ma, _ := strings.Cut(me, "/")
	mtool, _, _ := strings.Cut(ma, ".")
	keys := []string{"all", me, mh}
	for _, th := range []string{mh, "*"} {
		for _, ta := range []string{"*", ma, mtool} {
			keys = append(keys, th+"/"+ta)
		}
	}
	var idxs []int
	done := map[string]bool{}
	for _, k := range keys {
		if !done[k] {
			done[k] = true
			idxs = append(idxs, s.byTo[k]...)
		}
	}
	for to, sel := range s.sels {
		if sel != nil && sel.Match(labels) {
			idxs = append(idxs, s.byTo[to]...)
		}
	}
	slices.Sort(idxs)
	return slices.Compact(idxs)
}

// pruneSeenLocked drops sessions not heard from within seenKeep, at most once an
// hour, and, if the cap is exceeded, the oldest ones (down to 90% of it, so the
// work is not repeated on every call).
func (s *Store) pruneSeenLocked(now time.Time) {
	if len(s.seen) <= maxSeen && now.Sub(s.lastPrune) < time.Hour {
		return
	}
	s.lastPrune = now
	if now.Sub(s.lastDuePrune) >= time.Hour { // the roster over its cap makes every read come here: the due lists are not looked at each time
		s.lastDuePrune = now
		s.pruneDueLocked(now)
	}
	for k, v := range s.seen {
		if now.Sub(v.at) > seenKeep {
			delete(s.seen, k)
		}
	}
	if len(s.seen) <= maxSeen {
		return
	}
	keys := slices.SortedFunc(maps.Keys(s.seen), func(a, b string) int { return s.seen[a].at.Compare(s.seen[b].at) })
	for _, k := range keys[:len(keys)-maxSeen*9/10] {
		delete(s.seen, k)
	}
}

// Thread returns every message in the thread that message id belongs to.
func (s *Store) Thread(id string) []msg.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := id
	if m, ok := s.byIDLocked(id); ok {
		t = m.Thread
	}
	var out []msg.Message
	for _, i := range s.threads[t] {
		out = append(out, s.viewLocked(i)) // a retracted message stays in place, as a tombstone, so replies still make sense
	}
	return out
}

// Messages returns every message held, oldest first, each as a reader is shown it (a retracted one
// as its tombstone, a forged one without its words), for read-only views of the whole board.
func (s *Store) Messages() []msg.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]msg.Message, len(s.msgs))
	for i := range s.msgs {
		out[i] = s.viewLocked(i)
	}
	return out
}

// Readers is how many identities other than its sender acknowledged message id.
func (s *Store) Readers(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.byIDLocked(id)
	n := 0
	for by := range s.acks[id] {
		if !ok || by != m.From {
			n++
		}
	}
	return n
}

// Who maps each identity to when this node last heard from it.
func (s *Store) Who() map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range s.seen {
		out[k] = v.at
	}
	for from, at := range s.lastSent {
		if at.After(out[from]) {
			out[from] = at
		}
	}
	return out
}

// NoteTitle remembers title (already cleaned) as what me's session is called, for the roster. An
// empty title changes nothing, and a title for a session this node has not heard from yet counts as
// hearing from it: a session that only sends is in the roster with its name.
func (s *Store) NoteTitle(me, title string) {
	if title == "" || me == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	cur, known := s.seen[me]
	if known && cur.title == title {
		return
	}
	if !known {
		cur.at = now
	}
	cur.title = title
	s.seen[me] = cur
	s.pruneSeenLocked(now)
	s.presenceDirty = true
	s.flushPresenceLocked(now, false)
}

// Roster is Who with the labels each identity last declared to this node,
// sorted by identity. Identities known only from messages they sent have no labels.
func (s *Store) Roster() []msg.RosterEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	byMe := map[string]msg.RosterEntry{}
	now := s.now()
	for k, v := range s.seen {
		byMe[k] = msg.RosterEntry{Me: k, Seen: v.at, Labels: v.labels, Title: v.title, Listening: v.waitingUntil.After(now)}
	}
	for from, at := range s.lastSent {
		e := byMe[from]
		e.Me = from
		if at.After(e.Seen) {
			e.Seen = at
		}
		byMe[from] = e
	}
	out := make([]msg.RosterEntry, 0, len(byMe))
	for _, k := range slices.Sorted(maps.Keys(byMe)) {
		out = append(out, byMe[k])
	}
	return out
}

// Empty reports whether the store holds no record at all: a node on a new disk,
// which must hear from a peer before it takes writes (see Ready in the server).
func (s *Store) Empty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.log {
		if len(l) > 0 {
			return false
		}
	}
	return true
}

// Stats is a point-in-time summary of the store, for health and metrics.
type Stats struct {
	Messages    int
	Acks        int
	Sessions    int   // identities heard from by this node
	Duplicates  int64 // messages dropped as repeats since start
	RateLimited int64 // sends refused by the rate limit since start
	Quarantined int   // placeholders held, each in place of a record that failed msg.Record.ValidateReplicated
	Refused     int64 // records dropped for an origin that cannot be kept, since start

	Retracted              int // messages held that are retracted (hidden from inboxes, tombstones in threads)
	RetractionsIneffective int // retractions held for a message by an identity not on its sender's machine, which change nothing
}

// Stats summarises the store.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{Messages: len(s.msgs), Acks: s.nAck, Sessions: len(s.seen), Duplicates: s.dups, RateLimited: s.limited,
		Quarantined: s.quarantined, Refused: s.refused}
	st.Retracted, st.RetractionsIneffective = s.nRetracted, s.nInert // a claim on a message not held yet is neither: it is decided when the message arrives
	return st
}

// ── presence: who is around and what they work on ────────────────────────────
//
// A node learns a session's labels only when the session reads from it, so each
// node's view is partial. Nodes therefore gossip it (MergePresence, RosterSince)
// and keep it on disk, so a restart or a client that reads from another node does
// not empty the roster.

const (
	presenceFile       = "presence.json"
	presenceFlushEvery = 5 * time.Second
	maxClockAhead      = 5 * time.Minute // a sighting this far in the future is a wrong clock, not news
)

type presenceJSON struct {
	At     time.Time  `json:"at"`
	Labels msg.Labels `json:"labels,omitempty"`
	Title  string     `json:"title,omitempty"`
}

// loadPresence reads presence.json, dropping sessions already past seenKeep.
func (s *Store) loadPresence() {
	b, err := os.ReadFile(filepath.Join(s.dir, presenceFile))
	if err != nil {
		return
	}
	var m map[string]presenceJSON
	if json.Unmarshal(b, &m) != nil {
		return
	}
	now := s.now().UTC()
	for k, v := range m {
		if k != "" && now.Sub(v.At) <= seenKeep {
			s.seen[k] = seenInfo{at: v.At, labels: v.Labels, title: msg.CleanTitle(v.Title)}
		}
	}
}

// flushPresenceLocked writes presence.json if it changed, at most every
// presenceFlushEvery unless force (shutdown, tests). Best effort: a crash loses at
// most those seconds, which the next poll or gossip restores.
func (s *Store) flushPresenceLocked(_ time.Time, force bool) {
	if !s.presenceDirty || (!force && time.Since(s.presenceSaved) < presenceFlushEvery) {
		return
	}
	// A write that fails (disk full or read-only) is not retried on every read: the throttle
	// counts attempts, and the roster stays dirty to be tried again at the next interval.
	s.presenceSaved = time.Now()
	m := make(map[string]presenceJSON, len(s.seen))
	for k, v := range s.seen {
		m[k] = presenceJSON{At: v.at, Labels: v.labels, Title: v.title}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := filepath.Join(s.dir, presenceFile+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil && os.Rename(tmp, filepath.Join(s.dir, presenceFile)) == nil {
		s.presenceDirty = false
	}
}

// FlushPresence writes the roster to disk now (a graceful shutdown).
func (s *Store) FlushPresence() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushPresenceLocked(s.now().UTC(), true)
}

// MergePresence takes in what a peer knows about sessions: for each, the later
// sighting wins, and its labels with it unless it declared none (a peer that knows a
// session only from a message it sent has none to give). Sightings from the future
// are clamped, and sessions past seenKeep are ignored. It returns how many changed.
func (s *Store) MergePresence(entries []msg.RosterEntry) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	changed := 0
	for _, e := range entries {
		if e.Me == "" {
			continue
		}
		at := e.Seen
		if at.After(now.Add(maxClockAhead)) {
			at = now
		}
		if now.Sub(at) > seenKeep {
			continue
		}
		title := msg.CleanTitle(e.Title) // a peer's word, cleaned again
		cur, known := s.seen[e.Me]
		switch {
		case !known:
			s.seen[e.Me] = seenInfo{at: at, labels: e.Labels, title: title, waitingUntil: s.seen[e.Me].waitingUntil}
		case at.After(cur.at):
			labels := e.Labels
			if len(labels) == 0 {
				labels = cur.labels
			}
			if title == "" {
				title = cur.title
			}
			s.seen[e.Me] = seenInfo{at: at, labels: labels, title: title, waitingUntil: s.seen[e.Me].waitingUntil}
		case (len(cur.labels) == 0 && len(e.Labels) > 0) || (cur.title == "" && title != ""): // an older sighting that at least says what they work on, or what they call the session
			if len(cur.labels) == 0 {
				cur.labels = e.Labels
			}
			if cur.title == "" {
				cur.title = title
			}
			s.seen[e.Me] = cur
		default:
			continue
		}
		changed++
	}
	if changed > 0 {
		s.pruneSeenLocked(now)
		s.presenceDirty = true
		s.flushPresenceLocked(now, false)
	}
	return changed
}

// RosterSince is Roster limited to sessions seen after t, so a peer asking for
// news pays for what changed and not for the whole roster each time.
func (s *Store) RosterSince(t time.Time) []msg.RosterEntry {
	all := s.Roster()
	if t.IsZero() {
		return all
	}
	out := all[:0:0]
	for _, e := range all {
		if e.Seen.After(t) {
			out = append(out, e)
		}
	}
	return out
}
