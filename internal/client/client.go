// Package client talks to the aivoicebus nodes over HTTP: node failover, quorum
// writes, and one typed method per API call. It is the only package (besides
// the server) that knows the URL layout.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// ErrNoURL is returned by FromEnv when no node URL is configured.
var ErrNoURL = errors.New("AIVOICEBUS_URL not set (comma-separate several nodes): run `aivoicebus join` to connect this machine, or ask the user for the node URLs")

const (
	defaultTimeout = 8 * time.Second
	gatherTimeout  = 3 * time.Second // per node, when every node is asked at once
)

// StatusError is a node's definitive refusal of a request: a 4xx answer.
type StatusError struct {
	Code   int
	Status string // "429 Too Many Requests"
	Text   string // what the node said
	// Reason is why, as one msg.Refused* word: the node's msg.HeaderRefused, or what its status
	// and text add up to for a node from before that header ("" when unknown).
	Reason     string
	RetryAfter time.Duration // the node's Retry-After, when it gave one
}

// Error is one safe line that says what happened and what to do next, in this client's words for
// a reason it knows: a node is a peer, so its words never reach a terminal or an error object raw.
func (e *StatusError) Error() string {
	return e.explained(e.Refused())
}

// IsNotFound reports whether err is a node saying the endpoint does not exist,
// which is how a node from before the endpoint was added answers.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// unreachableError is "no node answered": the mesh is away, as opposed to a node saying no. It
// does not unwrap to the last failure: callers that look for a node's refusal must not find one here.
type unreachableError struct{ last error }

func (e *unreachableError) Error() string {
	return fmt.Sprintf("no node reachable: %v. Check the network, then run `aivoicebus doctor` (it says which node fails and why)", e.last)
}

func unreachable(last error) error { return &unreachableError{last} }

// IsUnreachable reports whether err is the failure of every node to answer (the exit code
// that tells a script to retry later rather than fix its request).
func IsUnreachable(err error) bool {
	var ue *unreachableError
	return errors.As(err, &ue)
}

// IsRejected reports whether err is a node refusing the token it was given (401), or
// refusing what the token may do (403). Either way this machine receives nothing until
// the token or the identity is fixed, which is not the same as the mesh being down.
func IsRejected(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && (se.Code == http.StatusUnauthorized || se.Code == http.StatusForbidden)
}

// Client calls a set of equivalent nodes.
type Client struct {
	URLs    []string      // nodes to try, in order
	Token   string        // shared bearer token
	Timeout time.Duration // per request; 0 means 8s
	// Deadline, when set, bounds the whole call across every node and request, on top of Timeout.
	Deadline time.Time
	Warn     io.Writer // where partial-write warnings go; nil means stderr
	// NoSign sends messages unsigned (a node replaying or relaying what others wrote, tests).
	NoSign bool

	// Labels are declared with every inbox read, so selector messages
	// ("@repo=x") reach this session and the roster shows what it works on.
	Labels msg.Labels
	// Title is the session's title to declare, when the caller knows it (the session-start hook);
	// empty means: whatever config.Title finds for the identity.
	Title string

	// Latest is the newest release number a node announced on any response so
	// far ("" if none did). It is a hint: verify the signed manifest before acting.
	Latest string

	// Server is what the node that answered last said about itself in its headers.
	// It is written by every call, so share a Client between goroutines only if
	// you do not read it.
	Server ServerInfo

	// HedgeDelay is how long a read waits on a node before also asking the next
	// one (the first answer wins, the other is cancelled); a node that fails is
	// replaced at once. 0 means 250ms. Writes are never hedged.
	HedgeDelay time.Duration
	// WriteQuorum is how many nodes a write must reach, sent in parallel; 0 means 2.
	WriteQuorum int

	hdr sync.Mutex // guards Latest and Server, which concurrent attempts both write
}

// ServerInfo is what a node says about itself on every answer.
type ServerInfo struct {
	Version string    // the release it runs ("" from a node that does not say)
	Date    time.Time // its clock, to the second
}

// Raw, passed as the out argument, receives a response body verbatim instead
// of decoding it as JSON. Limit bounds how much is read.
type Raw struct {
	Data  []byte
	Limit int64
}

// FromEnv builds a client from AIVOICEBUS_URL (or the env file) and the token.
func FromEnv() (*Client, error) {
	urls := config.URLs()
	if len(urls) == 0 {
		return nil, ErrNoURL
	}
	c := &Client{URLs: urls, Token: config.Token("")}
	if n, err := strconv.Atoi(os.Getenv("AIVOICEBUS_WRITE_QUORUM")); err == nil && n > 0 {
		c.WriteQuorum = n
	}
	if n, err := strconv.Atoi(os.Getenv("AIVOICEBUS_HEDGE_MS")); err == nil && n > 0 {
		c.HedgeDelay = time.Duration(n) * time.Millisecond
	}
	return c, nil
}

func (c *Client) hedgeDelay() time.Duration {
	if c.HedgeDelay > 0 {
		return c.HedgeDelay
	}
	return 250 * time.Millisecond
}

func (c *Client) writeQuorum() int {
	if c.WriteQuorum > 0 {
		return c.WriteQuorum
	}
	return 2
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return defaultTimeout
}

// once makes one attempt against one node. final reports that the outcome is
// definitive (success, or a 4xx such as a bad token) and must not fail over.
func (c *Client) once(base, method, path string, in, out any) (final bool, err error) {
	return c.onceCtx(context.Background(), base, method, path, in, out)
}

// attempt is a private copy for one concurrent attempt: attempts never share state, and only
// the winner's headers are adopted into c (in the caller's goroutine, before it returns), so a
// cancelled loser can never write c.Latest or c.Server after the call is over.
func (c *Client) attempt() *Client {
	return &Client{Token: c.Token, Timeout: c.Timeout, Deadline: c.Deadline}
}

func (c *Client) adopt(a *Client) {
	c.hdr.Lock()
	defer c.hdr.Unlock()
	if a.Latest != "" {
		c.Latest = a.Latest
	}
	if a.Server.Version != "" || !a.Server.Date.IsZero() {
		c.Server = a.Server
	}
}

// onceCtx is once that gives up when ctx is cancelled.
func (c *Client) onceCtx(ctx context.Context, base, method, path string, in, out any) (final bool, err error) {
	if !c.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, c.Deadline)
		defer cancel()
	}
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return false, fmt.Errorf("bad node url %q: %v", base, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: c.timeout()}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	c.hdr.Lock()
	if v := resp.Header.Get(msg.HeaderLatest); v != "" {
		c.Latest = v
	}
	c.Server = ServerInfo{Version: resp.Header.Get(msg.HeaderVersion)}
	if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
		c.Server.Date = d
	}
	c.hdr.Unlock()
	if resp.StatusCode >= 500 {
		return false, fmt.Errorf("%s: %s", base, resp.Status)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		text := strings.TrimSpace(string(b))
		retry, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return true, &StatusError{Code: resp.StatusCode, Status: resp.Status, Text: text,
			Reason: reasonOf(resp.StatusCode, resp.Header.Get(msg.HeaderRefused), text), RetryAfter: time.Duration(retry) * time.Second}
	}
	if out == nil {
		return true, nil
	}
	if raw, ok := out.(*Raw); ok {
		b, err := io.ReadAll(io.LimitReader(resp.Body, raw.Limit+1))
		if err != nil {
			return false, err // a body cut short is a network problem: try another node
		}
		if int64(len(b)) > raw.Limit {
			return true, fmt.Errorf("response is larger than %d bytes: ask the user to check the node (`aivoicebus nodes`)", raw.Limit)
		}
		raw.Data = b
		return true, nil
	}
	// A peer that answers with an endless array would otherwise be read into memory whole,
	// and a node does exactly that at start, before it listens: so a response has a size.
	lr := &io.LimitedReader{R: resp.Body, N: maxResponse + 1}
	derr := json.NewDecoder(lr).Decode(out)
	if lr.N <= 0 {
		return false, fmt.Errorf("%s: response is larger than %d bytes", base, maxResponse) // nextstep: one node's failure; unreachable() says what to do
	}
	return true, derr
}

// maxResponse is the most JSON a node's answer may hold. A legitimate sync page is far below it
// (a page is capped at about a megabyte, and a node that predates paging at 500 records).
const maxResponse = 16 << 20

// do asks the nodes until one answers. Network errors and 5xx fail over; 4xx (bad
// token, bad request) are final. A read that names several nodes is hedged: if
// the first has not answered within HedgeDelay the next is asked too, and the
// first answer wins, so one hung node costs a session start a quarter of a second
// and not its whole timeout. Anything else is tried one node at a time.
func (c *Client) do(urls []string, method, path string, in, out any) error {
	if len(urls) > 1 && method == http.MethodGet {
		return c.hedged(urls, method, path, in, out)
	}
	var last error
	for _, u := range urls {
		final, err := c.once(u, method, path, in, out)
		if final {
			return err
		}
		last = err
	}
	return unreachable(last)
}

// newOut returns an empty destination of the same kind as out, so concurrent
// attempts each decode into their own and only the winner is copied to out.
func newOut(out any) any {
	switch o := out.(type) {
	case nil:
		return nil
	case *Raw:
		return &Raw{Limit: o.Limit}
	}
	return reflect.New(reflect.TypeOf(out).Elem()).Interface()
}

// assignOut copies the winning attempt's result to out.
func assignOut(out, val any) {
	switch o := out.(type) {
	case nil:
	case *Raw:
		o.Data = val.(*Raw).Data
	default:
		reflect.ValueOf(out).Elem().Set(reflect.ValueOf(val).Elem())
	}
}

func (c *Client) hedged(urls []string, method, path string, in, out any) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		final bool
		err   error
		val   any
		from  *Client
	}
	results := make(chan result, len(urls))
	launch := func(u string) {
		val, from := newOut(out), c.attempt()
		go func() {
			final, err := from.onceCtx(ctx, u, method, path, in, val)
			results <- result{final, err, val, from}
		}()
	}
	next, inflight := 1, 1
	launch(urls[0])
	var last error
	for inflight > 0 {
		select {
		case r := <-results:
			inflight--
			if r.final {
				c.adopt(r.from)
				if r.err == nil {
					assignOut(out, r.val)
				}
				return r.err
			}
			last = r.err
			if next < len(urls) { // that node failed: do not wait to try the next
				launch(urls[next])
				next++
				inflight++
			}
		case <-time.After(c.hedgeDelay()):
			if next < len(urls) { // slow: ask the next as well and take whoever answers first
				launch(urls[next])
				next++
				inflight++
			}
		}
	}
	return unreachable(last)
}

// quorum POSTs to the nodes in parallel and succeeds once WriteQuorum of them
// (2 unless set) accepted it, or all reachable ones did if fewer are up, so a single
// node dying right after a write cannot strand it. A node that fails is replaced by
// the next one at once, and one that is merely slow by another after HedgeDelay.
// Servers dedupe by client-chosen id.
func (c *Client) quorum(path string, in, out any) error {
	return c.quorumWith(path, in, out, c.writeQuorum(), nil)
}

// quorumWith is quorum for a write that needs want nodes. soft, if given, is asked about each
// refusal a node makes (a 4xx): true says that node merely cannot take this write, and the next one
// is asked, as for a node that is down; false makes it final, as every refusal is for quorum.
func (c *Client) quorumWith(path string, in, out any, want int, soft func(error) bool) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		err  error
		val  any
		from *Client
	}
	results := make(chan result, len(c.URLs))
	next, inflight, ok := 0, 0, 0
	start := func() {
		u, val, from := c.URLs[next], newOut(out), c.attempt()
		next++
		inflight++
		go func() {
			_, err := from.onceCtx(ctx, u, http.MethodPost, path, in, val)
			results <- result{err, val, from}
		}()
	}
	for next < len(c.URLs) && inflight < want {
		start()
	}
	var last, refusal error
	var win any
	var winFrom *Client
	// A node that is slow rather than failed must not hold a write for its whole timeout: after
	// the hedge delay one more node is asked (writes are idempotent: servers dedupe by id), and
	// then another each delay, until the quorum is reached.
	hedge := time.NewTimer(c.hedgeDelay())
	defer hedge.Stop()
	for inflight > 0 {
		var r result
		select {
		case r = <-results:
		case <-hedge.C:
			if refusal == nil && next < len(c.URLs) {
				start()
			}
			hedge.Reset(c.hedgeDelay())
			continue
		}
		inflight--
		if r.err != nil {
			var refused *StatusError
			if errors.As(r.err, &refused) && (soft == nil || !soft(r.err)) {
				// A node refused the request itself (rate limit, bad request): the others
				// would too, or would let it through only to defeat the limit, so no more
				// nodes are asked. Attempts already under way are waited for, though: one
				// of them may have stored the message, and cancelling it would report a
				// refusal for a message that is on the board.
				if refusal == nil {
					refusal = r.err
				}
				continue
			}
			last = r.err
			if refusal == nil && ok+inflight < want && next < len(c.URLs) {
				start()
			}
			continue
		}
		ok++
		if win == nil {
			win, winFrom = r.val, r.from
		}
		if ok >= want {
			c.adopt(winFrom)
			assignOut(out, win)
			return nil
		}
	}
	if ok > 0 {
		c.adopt(winFrom)
		assignOut(out, win)
		w := c.Warn
		if w == nil {
			w = os.Stderr
		}
		fmt.Fprintf(w, "warning: only %d node(s) took the write\n", ok)
		return nil
	}
	switch {
	case refusal != nil:
		return refusal
	case last == nil:
		return ErrNoURL
	}
	return unreachable(last)
}

// Send posts a message (written to two nodes) and returns it as stored.
func (c *Client) Send(m msg.Message) (msg.Message, error) {
	if m.FromTitle == "" {
		m.FromTitle = c.titleFor(m.From) // the sender's session name now, kept with the message
	}
	if m.FromModel == "" {
		m.FromModel = msg.CleanModel(config.Model(m.From)) // and the model it runs
	}
	if m.Sig == "" && !c.NoSign {
		if priv, box, err := config.MachineKeys(); err == nil {
			m.Verified, m.Private = "", false
			m.Sign(priv, msg.BoxPublic(box)) // last: the signature covers the body as sent
		}
	}
	var out msg.Message
	err := c.quorum("/v1/send", m, &out)
	return out, err
}

// Retract withdraws message id, which by (an identity on the machine that sent it) takes back. leak
// flags the text as having held a credential; dryRun asks what would happen and writes nothing.
//
// The write goes to the nodes like any other (one node taking it is a success, and says when only
// one did), with two refusals that only mean "not this node": a node that does not hold the message
// yet answers 422, so the next one is asked, and the lookup is repeated a few times over about a
// second and a half for a message still replicating; a node that does not know retractions answers 404, and if no
// node that knows retractions took it, the retraction is written to the old ones as the plain ack
// those nodes take (id "retract:<id>"), which they replicate but do not apply. That result has State
// "recorded" and ReadBy -1: nothing is known of what the node did with it. Any other refusal (the
// sender's machine is another, a rate limit, a bad request) is final.
func (c *Client) Retract(id, by string, leak, dryRun bool) (msg.RetractResult, error) {
	req := msg.RetractRequest{ID: id, By: by, Leak: leak, DryRun: dryRun}
	var oldNode, unknown bool
	var unknownText string
	soft := func(err error) bool {
		var se *StatusError
		switch {
		case !errors.As(err, &se):
			return false
		case se.Code == http.StatusNotFound:
			oldNode = true
		case se.Code == http.StatusUnprocessableEntity:
			unknown, unknownText = true, se.Text
		default:
			return false
		}
		return true
	}
	want := c.writeQuorum()
	if dryRun {
		want = 1 // nothing is written, so there is nothing to make durable
	}
	for try := 0; ; try++ {
		oldNode, unknown = false, false
		var out msg.RetractResult
		err := c.quorumWith("/v1/retract", req, &out, want, soft)
		var refused *StatusError // quorumWith returns a refusal as it is only when it was final
		switch {
		case err == nil:
			return out, nil
		case errors.As(err, &refused):
			return msg.RetractResult{}, err
		case oldNode && dryRun: // an old node cannot say what would happen
			return msg.RetractResult{OK: true, State: msg.RetractDryRun, ReadBy: -1}, nil
		case oldNode:
			ackID := msg.RetractPrefix + id
			if leak {
				ackID = msg.RetractLeakPrefix + id
			}
			if err := c.quorumWith("/v1/ack", msg.AckRequest{ID: ackID, By: by}, nil, want, soft); err != nil {
				return msg.RetractResult{}, err
			}
			return msg.RetractResult{OK: true, State: msg.RetractRecorded, ReadBy: -1}, nil
		case unknown && try < 3: // the message may still be on its way to the nodes
			time.Sleep(2 * c.hedgeDelay())
		case unknown:
			return msg.RetractResult{}, &StatusError{Code: http.StatusUnprocessableEntity, Status: "422 Unprocessable Entity", Text: unknownText}
		default:
			return msg.RetractResult{}, err
		}
	}
}

// Inbox returns the messages addressed to me; all includes already-acked ones.
func (c *Client) Inbox(me string, all bool) ([]msg.Message, error) {
	q := "/v1/inbox?me=" + me
	if all {
		q += "&all=1"
	}
	if l := c.Labels.String(); l != "" {
		q += "&" + msg.ParamLabels + "=" + url.QueryEscape(l)
	}
	if t := c.titleFor(me); t != "" {
		q += "&" + msg.ParamTitle + "=" + url.QueryEscape(t)
	}
	var ms []msg.Message
	err := c.do(c.URLs, "GET", q, nil, &ms)
	Check(ms)
	return ms, err
}

// Check sets, on messages just read, what this machine finds of each signature (Verified, against
// the key pinned for the sender's machine: the first one seen is pinned), then opens a body sealed
// for this machine (Private). The signature covers the sealed text, so it is judged first. What a
// node said about either is never kept.
func Check(ms []msg.Message) {
	_, box, keyErr := config.MachineKeys()
	for i := range ms {
		m := &ms[i]
		m.Verified, m.Private = verdict(*m), false
		if keyErr == nil && msg.IsSealed(m.Body) {
			if pt, err := msg.Open(m.Body, box); err == nil {
				m.Body, m.Private = pt, true
			}
		}
	}
}

// verdict judges one message's signature.
func verdict(m msg.Message) string {
	if m.Retracted != nil || msg.IsSystemSender(m.From) { // a tombstone's body is the node's; a system notice is the node's own
		return ""
	}
	signed, valid := m.VerifySig()
	switch {
	case !signed:
		return msg.VerifiedUnsigned
	case !valid:
		return msg.VerifiedBad
	}
	host := msg.HostOf(m.From)
	key, box, known := config.KnownMachine(host)
	switch {
	case !known:
		_ = config.PinMachine(host, m.Key, m.Box)
		return msg.VerifiedSigned
	case key != m.Key:
		return msg.VerifiedChanged
	}
	if box == "" && m.Box != "" {
		_ = config.PinMachine(host, m.Key, m.Box)
	}
	return msg.VerifiedSigned
}

// InboxWait is Inbox held open by the node until something arrives that it had not got at gen,
// for at most wait (msg.FeatureInboxWait). With hasGen false it answers at once, and either way it
// returns the node's change counter for the next call. It asks the nodes in order and returns the
// first answer; any failure is returned as is, and the caller can fall back to Inbox.
func (c *Client) InboxWait(ctx context.Context, me string, gen uint64, hasGen bool, wait time.Duration) ([]msg.Message, uint64, error) {
	q := "/v1/inbox?me=" + url.QueryEscape(me)
	if l := c.Labels.String(); l != "" {
		q += "&" + msg.ParamLabels + "=" + url.QueryEscape(l)
	}
	if t := c.titleFor(me); t != "" {
		q += "&" + msg.ParamTitle + "=" + url.QueryEscape(t)
	}
	if hasGen && wait >= time.Second {
		q += fmt.Sprintf("&wait=%d&gen=%d", int(wait/time.Second), gen)
	}
	var last error = ErrNoURL
	for _, base := range c.URLs {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+q, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		resp, err := (&http.Client{Timeout: wait + 15*time.Second}).Do(req)
		if err != nil {
			last = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			last = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, 0, fmt.Errorf("%s answered %s", base, resp.Status) // nextstep: one node's failure; unreachable() says what to do
		}
		var ms []msg.Message
		if err := json.Unmarshal(body, &ms); err != nil {
			return nil, 0, fmt.Errorf("%s: %v", base, err)
		}
		g, err := strconv.ParseUint(resp.Header.Get(msg.HeaderGen), 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("%s does not hold reads open (no %s)", base, msg.HeaderGen) // nextstep: callers fall back to polling
		}
		Check(ms)
		return ms, g, nil
	}
	return nil, 0, last
}

// Leases lists the live leases a node holds (see msg.Holders for who holds what).
func (c *Client) Leases() ([]msg.Lease, error) {
	var ls []msg.Lease
	err := c.do(c.URLs, "GET", "/v1/leases", nil, &ls)
	return ls, err
}

// Receipts asks the nodes whether message id got through (see msg.Receipts).
func (c *Client) Receipts(id string) (msg.Receipts, error) {
	var r msg.Receipts
	err := c.do(c.URLs, "GET", "/v1/receipts?id="+url.QueryEscape(id), nil, &r)
	return r, err
}

// titleFor is the cleaned title this client declares for the session me: the one set on the client,
// else the session's own (AIVOICEBUS_TITLE, or what the session-start hook saved). "" declares none.
func (c *Client) titleFor(me string) string {
	if c.Title != "" {
		return msg.CleanTitle(c.Title)
	}
	return msg.CleanTitle(config.Title(me))
}

// Ack marks message id as read by `by` (written to two nodes).
func (c *Client) Ack(id, by string) error { return c.AckVia(id, by, "", 0) }

// AckVia is Ack with a delivery receipt: how the message reached `by` (one of
// msg.Via*) and how many characters it put into by's context.
func (c *Client) AckVia(id, by, via string, chars int) error {
	return c.quorum("/v1/ack", msg.AckRequest{ID: id, By: by, Via: via, Chars: chars}, nil)
}

// Thread returns every message in the thread message id belongs to.
func (c *Client) Thread(id string) ([]msg.Message, error) {
	var ms []msg.Message
	err := c.do(c.URLs, "GET", "/v1/thread?id="+id, nil, &ms)
	Check(ms)
	return ms, err
}

// gather asks every node for path at once and returns the answers of those that
// replied. If none did, it returns the first node's refusal (so a 404 from nodes
// that lack the endpoint can be told from a network failure) or "no node reachable".
func gather[T any](c *Client, path string) ([]T, error) {
	type result struct {
		v      T
		err    error
		latest string
	}
	rs := make([]result, len(c.URLs))
	var wg sync.WaitGroup
	for i, u := range c.URLs {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			one := &Client{Token: c.Token, Timeout: min(c.timeout(), gatherTimeout)}
			_, rs[i].err = one.once(u, "GET", path, nil, &rs[i].v)
			rs[i].latest = one.Latest
		}(i, u)
	}
	wg.Wait()
	var out []T
	var refusal, last error
	for _, r := range rs {
		if r.latest != "" {
			c.Latest = r.latest
		}
		var se *StatusError
		switch {
		case r.err == nil:
			out = append(out, r.v)
		case errors.As(r.err, &se):
			if refusal == nil {
				refusal = r.err
			}
		default:
			last = r.err
		}
	}
	switch {
	case len(out) > 0:
		return out, nil
	case refusal != nil:
		return nil, refusal
	case last != nil:
		return nil, unreachable(last)
	}
	return nil, errors.New("no node configured: run `aivoicebus join`, or ask the user for the node URLs")
}

// Roster lists the sessions the nodes have heard from, with the labels each
// declared. A node learns a session's labels only when the session reads from
// it, so every node is asked and the answers merged. Nodes without a roster
// answer 404 and are skipped; if none has one, the 404 is returned: use Who.
func (c *Client) Roster() ([]msg.RosterEntry, error) {
	all, err := c.RosterAll()
	if err != nil {
		return nil, err
	}
	var out []msg.RosterEntry
	for _, e := range all {
		if !msg.IsProbe(e.Me) { // a join probe is not a session (an older node still lists it)
			out = append(out, e)
		}
	}
	return out, nil
}

// RosterAll is Roster with every identity, join probes included. It is for decisions (who a
// message would reach, who holds a role): any session can name itself like a probe, and must
// still be counted there. Display views use Roster.
func (c *Client) RosterAll() ([]msg.RosterEntry, error) {
	lists, err := gather[[]msg.RosterEntry](c, "/v1/roster?all=1")
	if err != nil {
		return nil, err
	}
	return msg.MergeRosters(lists...), nil
}

// RosterSince is the roster of the nodes this client names, limited to sessions
// seen after t (a node asks its peers this way, for what changed).
func (c *Client) RosterSince(t time.Time) ([]msg.RosterEntry, error) {
	var r []msg.RosterEntry
	err := c.do(c.URLs, "GET", "/v1/roster?since="+url.QueryEscape(t.UTC().Format(time.RFC3339Nano)), nil, &r)
	return r, err
}

// Who maps each identity to when any node last heard from it.
func (c *Client) Who() (map[string]time.Time, error) {
	answers, err := gather[map[string]time.Time](c, "/v1/who")
	if err != nil {
		return nil, err
	}
	out := map[string]time.Time{}
	for _, a := range answers {
		for k, t := range a {
			if !msg.IsProbe(k) && t.After(out[k]) {
				out[k] = t
			}
		}
	}
	return out, nil
}

// TokenInfo is what a node says about the token it was asked with.
type TokenInfo struct {
	Name     string `json:"name"`
	Host     string `json:"host"`     // the one machine a machine token may act as ("" for a shared token)
	Observer bool   `json:"observer"` // read-only
}

// TokenInfo asks a node what kind of token this client holds. It is the cheapest call that
// tells a refused token (see IsRejected) from nodes nobody can reach. Nodes older than the
// release that added it answer 404 (see IsNotFound).
func (c *Client) TokenInfo() (TokenInfo, error) {
	var t TokenInfo
	err := c.do(c.URLs, "GET", "/v1/token", nil, &t)
	return t, err
}

// TokenSeen asks every node which kind of token last acted as host (msg.TokenSeen), and returns
// the answers of the nodes that gave one. Nodes older than msg.FeatureTokenSeen answer 404, which
// is returned when no node knows it (see IsNotFound).
func (c *Client) TokenSeen(host string) ([]msg.TokenSeen, error) {
	return gather[msg.TokenSeen](c, "/v1/token/seen?host="+url.QueryEscape(host))
}

// Unsigned asks a node who sent messages without a valid signature in the last days days
// (msg.UnsignedReport). Nodes older than msg.FeatureUnsigned answer 404 (see IsNotFound).
func (c *Client) Unsigned(days int) (msg.UnsignedReport, error) {
	var r msg.UnsignedReport
	err := c.do(c.URLs, "GET", "/v1/unsigned?days="+strconv.Itoa(days), nil, &r)
	return r, err
}

// Health is a node's own account of itself: see msg.Health.
// TokenKind asks a node what kind of token this client holds: "shared" (anyone holding it can send
// as any machine), "machine" (bound to host), or "observer"; "" from a node too old to say.
func (c *Client) TokenKind() (kind, host string, err error) {
	var r struct {
		Name, Host string
		Observer   bool
	}
	if err := c.do(c.URLs, "GET", "/v1/token", nil, &r); err != nil {
		return "", "", err
	}
	switch {
	case r.Observer:
		return "observer", "", nil
	case r.Host != "":
		return "machine", r.Host, nil
	case r.Name != "":
		return "shared", "", nil
	}
	return "", "", nil
}

func (c *Client) Health() (msg.Health, error) {
	var h msg.Health
	err := c.do(c.URLs, "GET", "/v1/health", nil, &h)
	cleanHealth(&h)
	return h, err
}

// cleanHealth makes every string a node chose one safe, capped line: a node is a peer, and what it
// says about itself reaches terminals (mesh, doctor, selftest) and JSON read by programs.
func cleanHealth(h *msg.Health) {
	h.Node, h.Version = view.OneLine(h.Node, 60), view.OneLine(h.Version, 40)
	for i, f := range h.Features {
		h.Features[i] = view.OneLine(f, 24)
	}
	for i := range h.Peers {
		p := &h.Peers[i]
		p.URL, p.Error, p.Version = view.OneLine(p.URL, 120), view.OneLine(p.Error, 200), view.OneLine(p.Version, 40)
	}
}

// Sync returns the records the answering node holds beyond version vector vv.
func (c *Client) Sync(vv map[string]uint64) ([]msg.Record, error) {
	b, _ := json.Marshal(vv)
	var recs []msg.Record
	err := c.do(c.URLs, "GET", "/v1/sync?vv="+url.QueryEscape(string(b)), nil, &recs)
	return recs, err
}

// SyncAll is Sync asked of every node: the union of what they hold beyond vv, ordered by
// origin and sequence. A backup must not stop because the node a hedged read happened to
// reach is behind; asking all of them lets the most complete one decide.
func (c *Client) SyncAll(vv map[string]uint64) ([]msg.Record, error) {
	b, _ := json.Marshal(vv)
	lists, err := gather[[]msg.Record](c, "/v1/sync?vv="+url.QueryEscape(string(b)))
	if err != nil {
		return nil, err
	}
	type key struct {
		origin string
		seq    uint64
	}
	seen := map[key]bool{}
	var out []msg.Record
	for _, l := range lists {
		for _, r := range l {
			if k := (key{r.Origin, r.Seq}); !seen[k] {
				seen[k] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Origin != out[j].Origin {
			return out[i].Origin < out[j].Origin
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// FetchRelease returns the named file of the release the nodes hold. Unlike
// other calls it fails over on every error, including 404: during a rolling
// deploy some nodes have the release before others do.
func (c *Client) FetchRelease(name string, limit int64) ([]byte, error) {
	var last error
	for _, u := range c.URLs {
		if !c.Deadline.IsZero() && time.Now().After(c.Deadline) {
			last = context.DeadlineExceeded
			break
		}
		raw := &Raw{Limit: limit}
		if err := c.do([]string{u}, "GET", "/v1/release/"+url.PathEscape(name), nil, raw); err != nil {
			last = err
			continue
		}
		return raw.Data, nil
	}
	if last == nil {
		last = errors.New("no node configured: run `aivoicebus join`, or ask the user for the node URLs")
	}
	return nil, last
}
