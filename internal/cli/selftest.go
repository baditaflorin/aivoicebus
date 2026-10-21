package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/attach"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// Registered here, not in cli.go's table, so adding it does not realign every line of that table
// (files initialise in name order: cli.go has made the map by now).
func init() { commands["selftest"] = selftestCmd }

// Runs allowed per machine per hour without --force: a script that loops selftest would otherwise
// leave a message and a tombstone in the log every time.
const (
	selftestMaxRuns = 6
	selftestWindow  = time.Hour
)

// selftestHop is one step of the check and how long it took.
type selftestHop struct {
	Name   string `json:"name"`
	Ms     int64  `json:"ms"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// selftestCmd is `aivoicebus selftest`: the same probe `join` uses (one note between two throwaway
// identities on this machine, through the real send, inbox and ack path), with each hop timed. On a
// mesh whose nodes all have the test lane the probe is a test message (msg/testlane.go): nothing is
// retracted, the node purges it, and one hop checks that a plain read does not show it. Without the
// lane, as before, it is retracted and shows as a tombstone. It never sends as this session and never
// writes to anyone else, so it is limited to selftestMaxRuns an hour per machine unless --force.
//
// --worker HOST and --file SIZE run scripted scenarios instead (selftest_scenarios.go), in the test lane.
func selftestCmd(args []string, s Streams) error {
	fs := newFlagSet("selftest")
	asJSON := fs.Bool("json", false, "print the hops as JSON")
	force := fs.Bool("force", false, "run even if it already ran "+strconv.Itoa(selftestMaxRuns)+" times in the last hour on this machine")
	workerHost := fs.String("worker", "", "check the worker on HOST (it must run with --test-lane): a calculation through the allowlist, signature and claim, then what the flags below ask for; every message is a test message, nothing is run, no tokens are spent")
	write := fs.Bool("write", false, "with --worker: ask what write mode would do with a task (it never asks for approval, never makes a worktree)")
	cancelIt := fs.Bool("cancel", false, "with --worker: send a long sleep, cancel it, and check the worker stops it within seconds")
	pauseIt := fs.Bool("pause", false, "with --worker: pause that worker only for a minute (pause --only), check a task waits, and that it is taken after (takes about 90 s)")
	brake := fs.Int("brake", 0, "with --worker: start a conversation limited to N messages (3 to 50), check the nodes refuse message N+1 and let an [end] through")
	fileSize := fs.String("file", "", "send a file of this size (300k, 1m) from one throwaway identity to another on this machine and check its sha256; needs --via")
	via := fs.String("via", "", "with --file: pipe (the pipe server, contacts it) or node (this mesh's nodes hold it)")
	timeout := fs.Duration("timeout", time.Minute, "with --worker or --file: how long each step waits")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: selftest [--json] [--force]   |   selftest --worker HOST [--write] [--cancel] [--pause] [--brake N]   |   selftest --file 300k --via pipe|node")
	}
	switch {
	case *workerHost != "" && *fileSize != "":
		return outbox.Usagef("--worker and --file are two scenarios: run it once for each")
	case *workerHost == "" && (*write || *cancelIt || *pauseIt || *brake != 0):
		return outbox.Usagef("--write, --cancel, --pause and --brake go with --worker HOST")
	case *brake != 0 && (*brake < 3 || *brake > 50):
		return outbox.Usagef("--brake is 3 to 50 messages: the task and its answer are two of them")
	case *fileSize != "" && *via != "pipe" && *via != "node":
		return outbox.Usagef("--file needs --via pipe or --via node: pipe contacts the pipe server (%s), node uses this mesh's nodes", attach.DefaultPipeURL)
	case *via != "" && *fileSize == "":
		return outbox.Usagef("--via goes with --file")
	case *timeout < time.Second:
		return outbox.Usagef("--timeout is at least 1s")
	}
	size := 0
	if *fileSize != "" {
		var err error
		if size, err = parseSize(*fileSize); err != nil {
			return err
		}
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	if !*force {
		if err := selftestAllowed(time.Now()); err != nil {
			return err
		}
	}
	title := "selftest (a probe between two throwaway identities on this machine)"
	var hops []selftestHop
	switch {
	case *workerHost != "":
		title = "selftest --worker " + view.OneLine(*workerHost, 60) + " (test messages from this machine's probe identity; nothing is run on the worker)"
		hops, err = runWorkerScenarios(c, *workerHost, workerOpts{write: *write, cancel: *cancelIt, pause: *pauseIt, brake: *brake}, *timeout)
	case *fileSize != "":
		title = "selftest --file " + *fileSize + " --via " + *via + " (a file between two throwaway identities on this machine)"
		hops, err = runFileScenario(c, size, *via, *timeout)
	default:
		hops, err = runSelftest(c)
	}
	if *asJSON {
		enc := json.NewEncoder(s.Out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			OK   bool          `json:"ok"`
			Hops []selftestHop `json:"hops"`
		}{err == nil, hops})
	} else {
		fmt.Fprintln(s.Out, title)
		var total int64
		for _, h := range hops {
			state := "ok  "
			if !h.OK {
				state = "FAIL"
			}
			total += h.Ms
			fmt.Fprintf(s.Out, "  %-8s %5dms  %s  %s\n", h.Name, h.Ms, state, h.Detail)
		}
		if err == nil {
			fmt.Fprintf(s.Out, "selftest passed (%dms)\n", total)
		}
	}
	if err != nil {
		fmt.Fprintln(s.Err, "selftest failed:", view.OneLine(err.Error(), 300))
		return exitError{1}
	}
	return nil
}

// selftestRunsPath is where the run times are kept; a variable so tests can point elsewhere.
var selftestRunsPath = func() string { return filepath.Join(config.Dir(), "selftest-runs") }

// selftestAllowed records this run and refuses the seventh within an hour. A state file that
// cannot be read or written never blocks a run: the limit is a loop guard, not a lock.
func selftestAllowed(now time.Time) error { return probeAllowed(selftestRunsPath(), now) }

// probeAllowed is the loop guard for any command that runs a probe: path keeps its run times.
func probeAllowed(path string, now time.Time) error {
	var recent []int64
	if b, err := os.ReadFile(path); err == nil {
		for _, f := range strings.Fields(string(b)) {
			if t, err := strconv.ParseInt(f, 10, 64); err == nil && now.Sub(time.Unix(t, 0)) < selftestWindow {
				recent = append(recent, t)
			}
		}
	}
	if len(recent) >= selftestMaxRuns {
		return fmt.Errorf("a probe already ran %d times in the last hour on this machine (each run leaves a message and a tombstone in the log); try again in an hour, or run it again with --force (selftest)", len(recent))
	}
	recent = append(recent, now.Unix())
	var b strings.Builder
	for _, t := range recent {
		b.WriteString(strconv.FormatInt(t, 10) + "\n")
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(b.String()), 0o600)
	return nil
}

// runSelftest runs the hops against c and returns them with the error that stopped it, if any.
func runSelftest(c *client.Client) ([]selftestHop, error) {
	var hops []selftestHop
	add := func(name string, d time.Duration, detail string, err error) error {
		h := selftestHop{Name: name, Ms: d.Milliseconds(), OK: err == nil, Detail: detail}
		if err != nil {
			h.Detail = view.OneLine(err.Error(), 200)
		}
		hops = append(hops, h)
		return err
	}

	t0 := time.Now()
	h, herr := c.Health()
	switch {
	case herr == nil:
		detail := "node " + view.OneLine(h.Node, 40) + " release " + view.OneLine(h.Version, 20)
		if len(h.Features) == 0 {
			detail += ", advertises no features"
		} else {
			detail += ", features " + featureList(&h)
		}
		_ = add("health", time.Since(t0), detail, nil)
	case client.IsNotFound(herr): // a node without /v1/health: the rest does not need it
		_ = add("health", time.Since(t0), "no /v1/health", nil)
	case client.IsRejected(herr):
		_ = add("health", time.Since(t0), "", herr)
		return hops, fmt.Errorf("the node refused this machine's token: %v", view.OneLine(herr.Error(), 120))
	default:
		_ = add("health", time.Since(t0), "", herr)
		return hops, errors.New("no node answered /v1/health: check the network, then run `aivoicebus doctor`")
	}

	lane := false // every node has the test lane: the probe is a test message, which needs no retraction
	if herr == nil {
		if old, lerr := c.TestLane(); lerr == nil && old == "" {
			lane = true
		}
	}
	var res probeResult
	var err error
	if lane {
		res, err = probeLaneRoundTrip(c)
	} else {
		res, err = probeRoundTrip(c)
	}
	if res.id != "" {
		_ = add("send", res.sent, "id "+view.OneLine(res.id, 12), nil)
		// the time until the reader's inbox showed it: polls, replication lag and the client's
		// hedge delay all count, so this is not the latency of one read
		vis := "the reader's inbox showed the probe intact (time to visible, polls included)"
		if err != nil {
			vis = ""
		}
		_ = add("visible", res.read, vis, err)
		if res.warnings == nil || !strings.Contains(strings.Join(res.warnings, " "), "acknowledging") {
			_ = add("ack", res.acked, "acknowledged as the reader", nil)
		}
		if res.lane && err == nil {
			_ = add("hidden", res.hidden, "a plain read of the reader does not show it (the node hides the test lane); the node purges it in about "+outbox.SpanShort(msg.TestLife), res.hiddenErr)
		}
		if res.retractState != "" {
			_ = add("retract", res.retracted, retractDetail(res.retractState), nil)
		}
		for _, w := range res.warnings {
			_ = add("cleanup", 0, "", errors.New(w))
		}
	} else if err != nil {
		_ = add("send", res.sent, "", err)
	}
	if err != nil {
		return hops, err
	}
	if res.hiddenErr != nil {
		return hops, res.hiddenErr
	}
	if len(res.warnings) > 0 {
		return hops, errors.New(res.warnings[0])
	}
	if !res.lane && (res.retractState == msg.RetractRetracted || res.retractState == msg.RetractAlready) {
		t := time.Now()
		ms, terr := c.Thread(res.id)
		var tomb bool
		for _, m := range ms {
			tomb = tomb || (m.ID == res.id && m.Retracted != nil && !strings.HasPrefix(m.Body, probeBodyPrefix))
		}
		if terr == nil && !tomb {
			terr = errors.New("the thread still shows the probe after the retraction: try again in a minute; if it stays, ask the user to check the nodes (`aivoicebus nodes`)")
		}
		if err := add("verify", time.Since(t), "the thread shows a tombstone, not the probe", terr); err != nil {
			return hops, err
		}
	}
	return hops, nil
}

func retractDetail(state string) string {
	if state == msg.RetractRecorded {
		return "recorded; this node cannot retract, the probe expires in " + strconv.Itoa(probeTTL) + " s"
	}
	return "retracted by the sender"
}
