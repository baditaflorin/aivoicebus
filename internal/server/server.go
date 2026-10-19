// Package server is a mesh node: the authenticated HTTP API over a store,
// optional bootstrap file serving, replication from peers, and node startup.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/secrets"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"github.com/baditaflorin/aivoicebus/internal/version"
)

// Option customises NewHandler.
type Option func(*handlerConfig)

type handlerConfig struct {
	// requireSigned refuses a message that carries no valid signature (serve --require-signed):
	// once every client signs (0.7.0 and later), an unsigned message is one that hides its machine.
	requireSigned bool
	releaseDir    string
	tokens        *TokenSet
	node          string
	version       string
	board         *PeerBoard
	metrics       *metrics
	ready         func() bool
}

// WithReleaseDir makes the node serve the release in dir (a manifest, its
// signature and the platform binaries) at /v1/release/<file>, and announce the
// release number on every authenticated response. The node never verifies or
// interprets the release beyond reading its version: clients check the signature.
func WithReleaseDir(dir string) Option { return func(c *handlerConfig) { c.releaseDir = dir } }

// WithObserverToken adds a second, read-only token for observers such as the
// dashboard. It may read the replication feed, the roster, threads, health
// and metrics, and nothing else: it can neither send, acknowledge, read an inbox
// (which would register a session) nor fetch a release. Holding it never lets
// anyone act as an agent, so a service that only watches the mesh should get
// this one and not the shared token. An empty token adds nothing.
func WithObserverToken(tok string) Option {
	return func(c *handlerConfig) { c.tokens.Add(tok, Principal{Name: "observer", Observer: true}) }
}

// WithTokenSet accepts every token in ts besides the one NewHandler was given.
func WithTokenSet(ts *TokenSet) Option {
	return func(c *handlerConfig) {
		if ts != nil {
			c.tokens.entries = append(c.tokens.entries, ts.entries...)
		}
	}
}

// WithNode names the node in /v1/health and /metrics.
func WithNode(id string) Option { return func(c *handlerConfig) { c.node = id } }

// WithRequireSigned refuses messages without a valid signature (join and selftest probes excepted).
func WithRequireSigned(on bool) Option { return func(c *handlerConfig) { c.requireSigned = on } }

// WithVersion overrides the release the node reports (tests; the default is this binary's).
func WithVersion(v string) Option { return func(c *handlerConfig) { c.version = v } }

// WithReady makes send and ack answer 503 until ready returns true. A node that
// starts on an empty disk must hear from a peer first (see Ready): it would
// otherwise number its records from 1 again and its peers, which still hold the old
// ones, would ignore every new record for good.
func WithReady(ready func() bool) Option { return func(c *handlerConfig) { c.ready = ready } }

// WithPeerBoard makes the node report how replication from its peers is going.
func WithPeerBoard(b *PeerBoard) Option { return func(c *handlerConfig) { c.board = b } }

func hostOf(identity string) string { return msg.HostOf(identity) }

// mayActAs reports whether the token's owner may send, acknowledge and read mail
// as identity: a machine token only as its own machine. This is all a machine token
// limits. It does not hide the board: like any agent token it can read threads, the
// roster and health; only the bulk replication feed (/v1/sync) is closed to it.
func (p Principal) mayActAs(identity string) bool { return p.Host == "" || hostOf(identity) == p.Host }

func (p Principal) deny(w http.ResponseWriter) {
	refuse(w, msg.RefusedDenied, fmt.Sprintf("this token may only act as %s/...: send as this machine, or ask the user for the token of the machine named", p.Host), http.StatusForbidden)
}

type principalHandler func(http.ResponseWriter, *http.Request, Principal)

// NewHandler is the node's API. Every route except /healthz needs a bearer token.
// token is the shared token; WithTokenSet and WithObserverToken add more.
func NewHandler(s *store.Store, token string, opts ...Option) http.Handler {
	cfg := handlerConfig{tokens: NewTokenSet(token), version: version.Release}
	for _, o := range opts {
		o(&cfg)
	}
	cfg.metrics = newMetrics(cfg.tokens.Names())
	leaks := cfg.tokens.Canaries()
	seen := newSeenBoard(filepath.Join(s.Dir(), "token-seen.json"))
	latest := &latestRelease{dir: cfg.releaseDir}
	mux := http.NewServeMux()
	// guardCap authenticates the request and, when observerOK, also lets a read-only
	// token through (for GET only). Every token is always compared, so the time
	// taken does not say which one was close. The request body is capped at maxBody.
	guardCap := func(observerOK, machineOK bool, maxBody int64, h principalHandler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := []byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			who, known := cfg.tokens.Lookup(got)
			switch {
			case !known:
				cfg.metrics.unauthorized.Add(1)
				refuse(w, msg.RefusedToken, "unauthorized: this node does not know the token; ask the user to check it (`aivoicebus doctor` says which token is in use)", http.StatusUnauthorized)
				return
			case who.Observer && !(observerOK && (r.Method == http.MethodGet || r.Method == http.MethodHead)):
				cfg.metrics.count(who.Name)
				refuse(w, msg.RefusedReadOnly, "the observer token is read-only and cannot do this: ask the user for the shared or a machine token", http.StatusForbidden)
				return
			case who.Host != "" && !machineOK:
				cfg.metrics.count(who.Name)
				refuse(w, msg.RefusedReadOnly, fmt.Sprintf("a machine token (%s/...) may not read this: ask the user to run it where the shared or observer token is", who.Host), http.StatusForbidden)
				return
			}
			cfg.metrics.count(who.Name)
			if who.Host != "" {
				seen.note(who.Host, who, "", r.Header.Get(msg.HeaderClient))
			}
			if v := latest.version(); v != "" {
				w.Header().Set(msg.HeaderLatest, v)
			}
			w.Header().Set(msg.HeaderVersion, cfg.version)
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
			h(w, r, who)
		}
	}
	guard := func(observerOK, machineOK bool, h principalHandler) http.HandlerFunc {
		return guardCap(observerOK, machineOK, msg.MaxBody+4096, h)
	}
	wrap := func(h principalHandler) http.HandlerFunc { return guard(false, true, h) }
	readonly := func(h principalHandler) http.HandlerFunc { return guard(true, true, h) }
	// feed is readonly for the replication feed, which holds every message to everyone: only
	// an unrestricted token or the observer may take it, not a token bound to one machine.
	feed := func(h principalHandler) http.HandlerFunc { return guard(true, false, h) }
	js := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	health := func() msg.Health {
		st := s.Stats()
		return msg.Health{Node: cfg.node, Version: cfg.version, Now: time.Now().UTC(), Log: s.VV(),
			Messages: st.Messages, Acks: st.Acks, Sessions: st.Sessions, Quarantined: st.Quarantined, Retracted: st.Retracted, Features: msg.NodeFeatures(), Peers: cfg.board.Snapshot()}
	}
	// retract is what /v1/retract and a /v1/ack of a retraction id both run, so the two cannot
	// differ in who may do it, in what is refused, or in what counts against the allowance. It
	// answers the request itself when it refuses, and says so by returning false.
	retract := func(w http.ResponseWriter, who Principal, req msg.RetractRequest) (msg.RetractResult, bool) {
		if !msg.ValidRetractID(req.ID) || !msg.ValidIdentity(req.By) || req.By == store.QuarantineBy {
			http.Error(w, "bad request: id must be a message id (ten lowercase hex digits) and by an identity", http.StatusBadRequest)
			return msg.RetractResult{}, false
		}
		if !who.mayActAs(req.By) {
			who.deny(w)
			return msg.RetractResult{}, false
		}
		var res msg.RetractResult
		var err error
		if req.DryRun {
			res, err = s.RetractDryRun(req.ID, req.By, req.Leak)
		} else {
			res, err = s.Retract(req.ID, req.By, req.Leak)
		}
		var notSender *store.ErrNotSender
		var limited *store.RateLimitError
		switch {
		case err == nil:
			return res, true
		case errors.Is(err, store.ErrUnknown): // never a retraction for a message this node has not got: another node may have it
			refuse(w, msg.RefusedUnknown, err.Error(), http.StatusUnprocessableEntity)
		case errors.As(err, &notSender):
			refuse(w, msg.RefusedNotSender, err.Error(), http.StatusForbidden)
		case errors.As(err, &limited):
			w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
			refuse(w, rateReason(limited), limited.Error(), http.StatusTooManyRequests)
		case errors.Is(err, store.ErrBadRetract):
			refuse(w, msg.RefusedBadRequest, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return msg.RetractResult{}, false
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("/v1/decide", decideHandler(s)) // no token: a one-time code from a phone button (decide.go)
	mux.HandleFunc("/v1/health", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) { js(w, health()) }))
	mux.HandleFunc("/metrics", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		cfg.metrics.write(w, health(), s.Stats())
	}))
	// What kind of token this is, so a script can tell a shared token from a machine's own before it
	// pushes it somewhere. Says nothing about the token itself.
	mux.HandleFunc("/v1/token", readonly(func(w http.ResponseWriter, r *http.Request, who Principal) {
		js(w, map[string]any{"name": who.Name, "host": who.Host, "observer": who.Observer})
	}))
	// Which kind of token last acted as a machine (seen.go): the proof that a machine uses its own.
	mux.HandleFunc("/v1/token/seen", readonly(seen.handler(cfg.node)))
	// Who sent messages without a valid signature lately (unsigned.go): ready for --require-signed?
	mux.HandleFunc("/v1/unsigned", readonly(unsignedHandler(s, cfg.node, cfg.requireSigned, time.Now)))
	mux.HandleFunc("/v1/send", wrap(func(w http.ResponseWriter, r *http.Request, who Principal) {
		if cfg.ready != nil && !cfg.ready() {
			refuse(w, msg.RefusedRecovering, "this node is recovering from its peers: try again in a minute, or use another node", http.StatusServiceUnavailable)
			return
		}
		var m msg.Message
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&m) != nil ||
			m.From == "" || m.To == "" || m.Body == "" || len(m.Body) > msg.MaxBody {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		m.Retracted = nil                 // what a node says about a message, never something a sender sets
		m.Verified, m.Private = "", false // what a reading client finds, never something a sender sets
		m.FromTitle = msg.CleanTitle(m.FromTitle)
		m.FromModel = msg.CleanModel(m.FromModel)
		if _, bad := leaks.Find(m.FromTitle); bad {
			m.FromTitle = "" // a title is shown to everyone: never one that holds a credential
		} else if _, bad := secrets.Strong(m.FromTitle); bad {
			m.FromTitle = ""
		}

		// the machine nodes speak as (retraction notices, which are made up and never stored), and
		// senders that could pass for it or for someone else
		if why := msg.SenderProblem(m.From); why != "" {
			http.Error(w, "bad request: from "+why, http.StatusBadRequest)
			return
		}
		if why := msg.TargetProblem(m.To); why != "" { // a target that only looks like another routes nowhere, or elsewhere
			http.Error(w, "bad request: to "+why, http.StatusBadRequest)
			return
		}
		if !who.mayActAs(m.From) {
			who.deny(w)
			return
		}
		if err := m.ValidateOptions(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := m.ValidateShape(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if cfg.requireSigned && !msg.IsProbe(m.From) {
			if signed, valid := m.VerifySig(); !signed || !valid {
				cfg.metrics.unsigned.Add(1)
				refuse(w, msg.RefusedUnsigned, "refused: this node takes only signed messages (serve --require-signed), and this one is unsigned or its signature does not verify: update the client with `aivoicebus update` (0.7.0 or later signs every message)", http.StatusForbidden)
				return
			}
		}
		// never store a credential, whichever client sent it: one of this node's own tokens
		// (matched exactly) or anything shaped like one
		f, found := leaks.Find(m.Body)
		if !found {
			f, found = secrets.Strong(m.Body)
		}
		if found {
			cfg.metrics.secrets.Add(1)
			refuse(w, msg.RefusedSecret, secrets.Refusal(f), http.StatusUnprocessableEntity)
			return
		}
		out, err := s.Send(m)
		var limited *store.RateLimitError
		switch {
		case errors.Is(err, store.ErrReservedID):
			refuse(w, msg.RefusedReservedID, "bad request: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrReplyToNotice):
			refuse(w, msg.RefusedNotice, "bad request: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, store.ErrTurnLimit):
			refuse(w, turnReason(err), err.Error(), http.StatusConflict)
		case errors.As(err, &limited):
			w.Header().Set("Retry-After", strconv.Itoa(int(limited.RetryAfter.Seconds())+1))
			refuse(w, rateReason(limited), limited.Error(), http.StatusTooManyRequests)
		case err != nil:
			http.Error(w, err.Error(), 500)
		default:
			s.NoteTitle(out.From, out.FromTitle)
			seen.note(hostOf(out.From), who, out.ID, r.Header.Get(msg.HeaderClient))
			js(w, out)
		}
	}))
	mux.HandleFunc("/v1/inbox", wrap(func(w http.ResponseWriter, r *http.Request, who Principal) {
		me := r.URL.Query().Get("me")
		if !msg.ValidIdentity(me) {
			http.Error(w, "me required (at most 200 characters, no control characters)", 400)
			return
		}
		if !who.mayActAs(me) {
			who.deny(w)
			return
		}
		seen.note(hostOf(me), who, "", r.Header.Get(msg.HeaderClient))
		if r.URL.Query().Get("peek") == "1" { // msg.FeatureInboxPeek: a look on someone's behalf, never a session's own read
			js(w, s.Peek(me))
			return
		}
		// Labels are optional and tolerant: junk in the parameter must never
		// break a plain inbox read, so bad pairs are simply dropped.
		labels := msg.ParseLabels(r.URL.Query().Get(msg.ParamLabels))
		if who.Host != "" { // the machine a token is bound to is not for the client to declare
			labels["machine"] = hostOf(me)
		}
		all := r.URL.Query().Get("all") == "1"
		// A long poll (wait=S&gen=G, see msg.FeatureInboxWait): when nothing has changed on this node
		// since the reader's last look (gen), hold the read until something does, at most S seconds.
		// The session counts as listening meanwhile, so `who` and `ping` can say so.
		if wait, err := strconv.Atoi(r.URL.Query().Get("wait")); err == nil && wait > 0 && !all {
			d := min(time.Duration(wait)*time.Second, msg.MaxInboxWait)
			g, changed := s.Gen()
			if seen, err := strconv.ParseUint(r.URL.Query().Get("gen"), 10, 64); err == nil && seen == g {
				s.InboxFor(me, false, labels) // registers the session before it is marked listening
				s.NoteWaiting(me, time.Now().Add(d))
				t := time.NewTimer(d)
				select {
				case <-changed:
				case <-t.C:
				case <-r.Context().Done():
				}
				t.Stop()
				s.NoteWaiting(me, time.Time{})
			}
		}
		g, _ := s.Gen() // before the read: something arriving in between is then both in it and newer
		inbox := s.InboxFor(me, all, labels)
		w.Header().Set(msg.HeaderGen, strconv.FormatUint(g, 10))
		if t := msg.CleanTitle(r.URL.Query().Get(msg.ParamTitle)); t != "" {
			if _, bad := secrets.Strong(t); !bad {
				s.NoteTitle(me, t) // after the read, which keeps the last title when none is declared
			}
		}
		js(w, inbox)
	}))
	mux.HandleFunc("/v1/ack", wrap(func(w http.ResponseWriter, r *http.Request, who Principal) {
		if cfg.ready != nil && !cfg.ready() {
			refuse(w, msg.RefusedRecovering, "this node is recovering from its peers: try again in a minute, or use another node", http.StatusServiceUnavailable)
			return
		}
		var a msg.AckRequest
		if json.NewDecoder(r.Body).Decode(&a) != nil || !msg.ValidRef(a.ID) || !msg.ValidIdentity(a.By) || a.By == store.QuarantineBy || !msg.ValidVia(a.Via) {
			http.Error(w, "bad request", 400)
			return
		}
		if !who.mayActAs(a.By) {
			who.deny(w)
			return
		}
		if msg.HasRetractPrefix(a.ID) { // a retraction written the way a client that predates /v1/retract writes it (aivoicebus ack retract:<id>)
			mid, leak, ok := msg.RetractedID(a.ID)
			if !ok {
				http.Error(w, "bad request: a retraction names a message by its id (ten lowercase hex digits)", http.StatusBadRequest)
				return
			}
			if _, done := retract(w, who, msg.RetractRequest{ID: mid, By: a.By, Leak: leak}); done {
				js(w, map[string]bool{"ok": true})
			}
			return
		}
		a.Chars = min(max(a.Chars, 0), msg.MaxBody)
		if err := s.AckWith(msg.Ack{ID: a.ID, By: a.By, Via: a.Via, Chars: a.Chars}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		js(w, map[string]bool{"ok": true})
	}))
	mux.HandleFunc("/v1/retract", wrap(func(w http.ResponseWriter, r *http.Request, who Principal) {
		if cfg.ready != nil && !cfg.ready() {
			refuse(w, msg.RefusedRecovering, "this node is recovering from its peers: try again in a minute, or use another node", http.StatusServiceUnavailable)
			return
		}
		var req msg.RetractRequest
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if res, done := retract(w, who, req); done {
			js(w, res)
		}
	}))
	mux.HandleFunc("/v1/thread", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		js(w, s.Thread(r.URL.Query().Get("id")))
	}))
	mux.HandleFunc("/v1/sync", feed(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		vv := map[string]uint64{}
		_ = json.Unmarshal([]byte(r.URL.Query().Get("vv")), &vv)
		js(w, s.SinceBytes(vv, syncPageRecords, syncPageBytes))
	}))
	// Who and the roster are display views: join probes are left out unless ?all=1 asks for every
	// identity (the sender's own safety checks, which count recipients, must not miss a session that
	// merely named itself like a probe), and gossip (?since) always carries everything.
	mux.HandleFunc("/v1/leases", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		js(w, s.Leases())
	}))
	mux.HandleFunc("/v1/receipts", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		id := r.URL.Query().Get("id")
		if !msg.ValidRef(id) {
			http.Error(w, "id required", http.StatusBadRequest)
			return
		}
		rc, ok := s.Receipts(id)
		if !ok {
			http.Error(w, "message not found on this node", http.StatusNotFound)
			return
		}
		js(w, rc)
	}))
	mux.HandleFunc("/v1/who", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		out := s.Who()
		if r.URL.Query().Get("all") != "1" {
			for k := range out {
				if msg.IsProbe(k) {
					delete(out, k)
				}
			}
		}
		js(w, out)
	}))
	mux.HandleFunc("/v1/roster", readonly(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		var since time.Time
		if v := r.URL.Query().Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339Nano, v)
			if err != nil {
				http.Error(w, "since must be an RFC 3339 time", http.StatusBadRequest)
				return
			}
			since = t
		}
		out := s.RosterSince(since)
		if since.IsZero() && r.URL.Query().Get("since") == "" && r.URL.Query().Get("all") != "1" {
			out = slices.DeleteFunc(out, func(e msg.RosterEntry) bool { return msg.IsProbe(e.Me) })
		}
		js(w, out)
	}))
	// Files: the agent tokens only (never the observer), the upload with its own larger cap.
	fileRoutes(mux, newFileBox(), func(h principalHandler) http.HandlerFunc { return guardCap(false, true, MaxFileEnvelope, h) }, wrap)
	mux.HandleFunc("/v1/release/", wrap(func(w http.ResponseWriter, r *http.Request, _ Principal) {
		name := strings.TrimPrefix(r.URL.Path, "/v1/release/")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if cfg.releaseDir == "" || !release.ValidName(name) {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(cfg.releaseDir, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		longWrite(w)
		http.ServeContent(w, r, name, fi.ModTime(), f)
	}))
	return mux
}
