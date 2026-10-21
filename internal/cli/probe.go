package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// probeTTL is how long a probe note lives if cleaning it up fails, in seconds.
const probeTTL = 300

// probeBodyPrefix starts what every probe says, for `join` and `selftest` alike: anyone who meets one
// in a log knows it is a machine check. A short random nonce follows, so two probes within the
// nodes' duplicate-suppression window are never the same message (a node that predates the duplicate rule would answer
// the second with the first one's already-acknowledged id, and the check would fail for nothing).
const probeBodyPrefix = "aivoicebus probe"

func newProbeBody() string {
	return probeBodyPrefix + " " + msg.NewID()[:6] + " (join or selftest): this machine can send and read. Safe to ignore."
}

// probeResult is what the round trip found, cleanup included: what did not get cleaned up is said.
type probeResult struct {
	id                           string
	sent, read, acked, retracted time.Duration // read is the time until the probe was visible, polls included
	retractState                 string        // msg.Retract*; "" when the retraction failed
	warnings                     []string
	// lane is set for a probe sent in the test lane (probeLaneRoundTrip): nothing is retracted, the node
	// purges it, and hidden is how long the check took that a read that did not ask for the lane never shows it.
	lane      bool
	hidden    time.Duration
	hiddenErr error // the plain read showed the probe (or could not be made): the nodes do not hide the lane
}

func (r probeResult) clean() bool { return len(r.warnings) == 0 }

// probeRoundTrip sends a short-lived note from one throwaway identity on this machine to another,
// reads it from the second one's inbox (a session never sees its own messages) and checks the
// body, acknowledges it, and retracts it. It is the one probe both `join` and `selftest` use. The
// identities use the host part real sessions use (a machine token is bound to it, nothing else
// of the caller's identity is touched, and the real session's send allowance is not spent) and a
// reserved agent name the roster views leave out. Whatever cleanup fails is reported.
func probeRoundTrip(c *client.Client) (probeResult, error) {
	var res probeResult
	host := identity.Label()
	from, reader := host+"/"+msg.ProbeSenderAgent, host+"/"+msg.ProbeReaderAgent
	m := msg.Message{ID: msg.NewID(), From: from, To: reader, Prio: msg.PrioFYI, TTL: probeTTL, Body: newProbeBody()}
	t0 := time.Now()
	noSign := c.NoSign
	c.NoSign = true // a probe is withdrawn at once; unsigned, it is the same on a node of any release
	out, err := c.Send(m)
	c.NoSign = noSign
	if err != nil {
		return res, fmt.Errorf("the mesh took the token but refused the probe message: %w", err)
	}
	res.id = out.ID
	res.sent = time.Since(t0)
	t1 := time.Now()
	found, wrong := false, false
	var readErr error
	for try := 0; try < 6 && !found; try++ { // the write reaches two nodes; a read may land on the other first
		if try > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		ms, rerr := c.Inbox(reader, false)
		if readErr = rerr; rerr != nil {
			continue
		}
		for _, got := range ms {
			if got.ID != out.ID {
				continue
			}
			if got.Body != m.Body {
				wrong = true
			}
			found = true
		}
	}
	res.read = time.Since(t1)
	// cleanup whatever happened to the read: the probe must not linger
	left := fmt.Sprintf("the probe note is left to expire in %d s", probeTTL)
	t2 := time.Now()
	if err := c.AckVia(out.ID, reader, msg.ViaCLI, 0); err != nil {
		res.warnings = append(res.warnings, fmt.Sprintf("acknowledging the probe failed (%v); %s", err, left))
	}
	res.acked = time.Since(t2)
	t3 := time.Now()
	if r, err := c.Retract(out.ID, from, false, false); err != nil {
		res.warnings = append(res.warnings, fmt.Sprintf("retracting the probe failed (%v); %s", err, left))
	} else {
		res.retractState = r.State
	}
	res.retracted = time.Since(t3)
	switch {
	case wrong:
		return res, errors.New("the probe came back from the mesh with a different body than was sent: ask the user to check the nodes (`aivoicebus nodes`)")
	case !found && readErr != nil:
		return res, fmt.Errorf("the probe message was sent but reading it failed: %v", readErr)
	case !found:
		return res, errors.New("the probe message was sent but did not come back from the mesh within a few seconds: try again in a minute, then run `aivoicebus doctor`")
	}
	return res, nil
}

// probeLaneRoundTrip is probeRoundTrip in the test lane (msg/testlane.go), for a mesh whose nodes all have it:
// the probe is a test message between the two probe identities, read by the second one with a read that
// asks for the lane and declares the opt-in, acknowledged, and then looked for with a read that did not
// ask for the lane (the other conditions as they were), which must not show it: that is the nodes hiding
// it, checked on this very mesh. Nothing is retracted: the node purges a test message by itself.
func probeLaneRoundTrip(c *client.Client) (probeResult, error) {
	res := probeResult{lane: true}
	host := identity.Label()
	from, reader := host+"/"+msg.ProbeSenderAgent, host+"/"+msg.ProbeReaderAgent
	optIn := msg.Labels{msg.TestLabel: msg.TestOn} // what a session that opted in declares with every read
	lc := c.WithLane()
	lc.Labels = optIn
	m := msg.Message{ID: msg.NewTestID(), From: from, To: reader, Prio: msg.PrioFYI, TTL: probeTTL, Body: newProbeBody()}
	t0 := time.Now()
	noSign := c.NoSign
	c.NoSign = true // as probeRoundTrip: unsigned is the same on a node of any release
	out, err := c.Send(m)
	c.NoSign = noSign
	if err != nil {
		return res, fmt.Errorf("the mesh took the token but refused the probe message: %w", err)
	}
	res.id = out.ID
	res.sent = time.Since(t0)
	t1 := time.Now()
	found, wrong := false, false
	var readErr error
	for try := 0; try < 6 && !found; try++ {
		if try > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		ms, rerr := lc.Inbox(reader, false)
		if readErr = rerr; rerr != nil {
			continue
		}
		for _, got := range ms {
			if got.ID != out.ID {
				continue
			}
			wrong = wrong || got.Body != m.Body
			found = true
		}
	}
	res.read = time.Since(t1)
	t2 := time.Now()
	if err := c.AckVia(out.ID, reader, msg.ViaCLI, 0); err != nil {
		res.warnings = append(res.warnings, fmt.Sprintf("acknowledging the probe failed (%v); the node purges it within about %s", err, outbox.SpanShort(msg.TestLife)))
	}
	res.acked = time.Since(t2)
	switch {
	case wrong:
		return res, errors.New("the probe came back from the mesh with a different body than was sent: ask the user to check the nodes (`aivoicebus nodes`)")
	case !found && readErr != nil:
		return res, fmt.Errorf("the probe message was sent but reading it failed: %v", readErr)
	case !found:
		return res, errors.New("the probe message was sent but did not come back from the mesh within a few seconds: try again in a minute, then run `aivoicebus doctor`")
	}
	// The hiding itself: the same reader, with the opt-in declared, but with a read that does not ask for the lane.
	t3 := time.Now()
	plain := c.WithRaw(false)
	plain.Labels = optIn
	ms, rerr := plain.Inbox(reader, true)
	if rerr != nil {
		res.hiddenErr = fmt.Errorf("the check that a plain read does not show the probe failed: %v", rerr)
		return res, nil
	}
	th, terr := plain.Thread(out.ID)
	if terr != nil {
		res.hiddenErr = fmt.Errorf("the check that a plain read does not show the probe failed: %v", terr)
		return res, nil
	}
	res.hidden = time.Since(t3)
	for _, x := range append(ms, th...) {
		if x.ID == out.ID {
			res.hiddenErr = errors.New("a node showed the test probe to a read that did not ask for the test lane: it does not hide the lane, so a test message would reach sessions that never opted in; ask the user to check the nodes (`aivoicebus nodes`)")
			return res, nil
		}
	}
	return res, nil
}
