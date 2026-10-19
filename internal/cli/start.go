package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/credstore"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/tasks"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// start is the guided first run: it reaches the mesh, sets up each tool it finds, has the person
// trust the Codex hooks and make an approver key, takes an optional phone alert URL, runs doctor,
// and then says hello to a session that is listening. Every step reuses the command that does it
// alone (join, setup, trust-codex, approver init, watch notify, doctor, send), says what it did
// and what is left, and skips what is done, so running it again is safe. With --yes it asks
// nothing; in an agent's session it never does what only the person may (type the mesh token,
// trust hooks, choose a passphrase, give a notify URL): those are listed for the person instead.

// startTerminal says whether start can ask its questions; a variable so tests can answer them.
var startTerminal = func(s Streams) bool { _, ok := terminalFile(s.In); return ok }

// startTrustCodex is trust-codex as start runs it; a variable so tests start no Codex.
var startTrustCodex = trustCodexCmd

// joinLinkMax bounds a join link's download: the bundle holds two binaries besides the token.
const joinLinkMax = 256 << 20

type startRun struct {
	s         Streams
	yes       bool // --yes: ask nothing, take the defaults
	agent     bool // an agent's session runs it: the person-only steps are refused
	ask       bool // there is a person at a terminal to answer
	left      [][2]string
	failed    bool
	meshReady bool
}

func (r *startRun) line(status, name, detail string) {
	fmt.Fprintf(r.s.Out, "%-12s %-22s %s\n", status, name, detail)
}

// leave records a step the person still has to do, as the command that does it and why.
func (r *startRun) leave(cmd, why string) { r.left = append(r.left, [2]string{cmd, why}) }

// refusedForAgent is the line for a person-only step an agent's session reached.
func (r *startRun) refusedForAgent(name, what, cmd string) {
	r.line("refused", name, what+" is the person's to do, not an agent's: they run `"+cmd+"` in their own terminal")
	r.leave(cmd, what)
}

// confirm asks a yes/no question whose answer defaults to yes.
func (r *startRun) confirm(prompt string) bool {
	got, err := askLine(r.s, prompt+" [Y/n] ")
	if err != nil {
		return false
	}
	got = strings.ToLower(got)
	return got == "" || got == "y" || got == "yes"
}

func startCmd(args []string, s Streams) error {
	fs := newFlagSet("start")
	yes := fs.Bool("yes", false, "ask nothing and take the defaults; the steps a person must type (token, Codex trust, approver passphrase) are listed as left instead")
	urls := fs.String("url", "", "node URLs (comma-separated) when this machine has none yet; the token is asked for, or piped in")
	trust := fs.Bool("trust-url", false, "with --url: allow nodes this machine has no signed list for (you type each host:port to confirm, as join asks)")
	tools := fs.String("tools", "", "tools to set up: claude,codex,gemini (default: all that are installed)")
	helloWait := fs.Duration("hello-wait", time.Minute, "how long to wait for the hello's answer")
	noHello := fs.Bool("no-hello", false, "do not say hello to a listening session at the end")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: aivoicebus start [--yes] [--url URLS [--trust-url]] [--tools LIST] [--hello-wait 1m] [--no-hello]   (a join link or the token is pasted at its prompt, or piped in)")
	}
	r := &startRun{s: s, yes: *yes, agent: os.Getenv("AIVOICEBUS_AGENT") != ""}
	r.ask = !r.yes && !r.agent && startTerminal(s)
	if !r.yes && !r.agent && !r.ask {
		return outbox.Usagef("start asks you a few questions and there is no terminal to ask on: run it in a terminal, or pass --yes for the defaults")
	}
	fmt.Fprintln(s.Out, "aivoicebus start: each step says what it did and what is left; running it again skips what is done.")
	if r.agent {
		fmt.Fprintln(s.Out, "(an agent's session runs this: the steps only the person may do are listed for them, not done)")
	}

	began := time.Now()
	r.mesh(*urls, *trust, *tools)
	r.setup(*tools)
	r.codexTrust()
	r.approver()
	r.notify()
	r.doctor()
	if !*noHello {
		r.hello(*helloWait)
	}
	r.summary(time.Since(began))
	if r.failed {
		return exitError{exitFailure}
	}
	return nil
}

// mesh makes sure this machine reaches the mesh: an existing configuration that works is kept;
// otherwise a join link, or node URLs and the token, go through join.
func (r *startRun) mesh(flagURLs string, trust bool, tools string) {
	r.step("1. mesh")
	configured := configuredURLs()
	_, haveTok := readSavedToken(config.TokenPath(""))
	haveTok = haveTok || os.Getenv("AIVOICEBUS_TOKEN") != ""
	replace := false
	if flagURLs == "" && len(configured) > 0 && haveTok {
		_, err := meshProbe()
		switch {
		case err == nil:
			r.line("ok", "mesh", strings.Join(configured, ",")+" answers and takes this machine's token")
			r.meshReady = true
			return
		case errors.Is(err, agentcfg.ErrTokenRejected):
			r.line("FAIL", "mesh", "the nodes refuse this machine's token: "+view.OneLine(err.Error(), 120))
			replace = true
		default:
			r.line("FAIL", "mesh", "configured ("+strings.Join(configured, ",")+"), but no node answers: "+view.OneLine(err.Error(), 120))
			r.leave("aivoicebus doctor", "check the network or the node URLs, then run start again")
			r.failed = true
			return
		}
	}
	if r.agent {
		r.refusedForAgent("mesh", "typing the mesh token", "aivoicebus start")
		r.failed = true
		return
	}
	o := joinOpts{tools: tools, trust: trust, replace: replace}
	switch {
	case flagURLs != "":
		o.urls = flagURLs
	case r.yes:
		piped := ""
		if in := stdinOrNil(r.s.In); in != nil {
			b, _ := io.ReadAll(io.LimitReader(in, credstore.MaxLen+2))
			piped = strings.TrimSpace(string(b))
		}
		switch {
		case isJoinLink(piped):
			if !r.fromLink(piped, &o) {
				return
			}
		case len(configured) > 0 && piped != "":
			o.urls, o.token, o.tokenFrom = strings.Join(configured, ","), piped, "piped in"
		default:
			r.line("left", "mesh", "no mesh configured yet, and --yes asks nothing")
			r.leave("aivoicebus start", "in a terminal: it asks for a join link, or the node URL and the token (or: pbpaste | aivoicebus start --yes --url URL)")
			r.failed = true
			return
		}
	case len(configured) > 0:
		o.urls = strings.Join(configured, ",") // the token is missing or refused: join asks for it
	default:
		fmt.Fprintln(r.s.Out, "This machine is not on a mesh yet. You need either a join link from whoever runs it")
		fmt.Fprintln(r.s.Out, "(http://NODE:7777/bootstrap/KEY.tgz), or the node URL(s) and the mesh token.")
		got, err := askLine(r.s, "Node URL(s), comma-separated (or press Enter to paste a join link): ")
		if err != nil {
			r.line("FAIL", "mesh", err.Error())
			r.failed = true
			return
		}
		if got == "" {
			link, err := readPassphrase("Join link (paste it; nothing is shown): ")
			if err != nil {
				r.line("FAIL", "mesh", err.Error())
				r.failed = true
				return
			}
			if !r.fromLink(strings.TrimSpace(link), &o) {
				return
			}
		} else {
			o.urls, o.typed, o.nodesFrom = got, true, "as you typed them"
		}
	}
	if err := joinRun(o, r.s); err != nil {
		r.line("FAIL", "mesh", "join did not finish (above); nothing it had not proved was written")
		r.leave("aivoicebus start", "again, once the reason above is fixed")
		r.failed = true
		return
	}
	config.LoadEnvFile() // join wrote the env file: the steps below read the nodes from it
	r.line("done", "mesh", "joined: this machine sends and reads")
	r.meshReady = true
}

// fromLink fills o from a join link (deploy/bootstrap-share.sh serves it): the node URLs and the
// token inside the bundle. The link is a secret and is never printed; only its host is.
func (r *startRun) fromLink(link string, o *joinOpts) bool {
	urls, tok, err := fetchJoinLink(link)
	if err != nil {
		r.line("FAIL", "mesh", "join link: "+err.Error())
		r.leave("aivoicebus start", "again with a fresh link (one is served for 24h), or with the node URL and the token")
		r.failed = true
		return false
	}
	o.urls, o.token, o.tokenFrom, o.typed, o.nodesFrom, o.replace = strings.Join(urls, ","), tok, "from the join link", true, "from the join link", true
	return true
}

// isJoinLink says whether text is a bootstrap link rather than a token.
func isJoinLink(text string) bool {
	u, err := url.Parse(text)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.HasPrefix(u.Path, "/bootstrap/") && strings.HasSuffix(u.Path, ".tgz")
}

// fetchJoinLink downloads a client bundle and takes out the node URL list and the token; the
// binaries in it are left alone (this binary is already here). Plain http goes only to a private
// address, the rule join applies to the token itself.
func fetchJoinLink(link string) (urls []string, token string, err error) {
	if !isJoinLink(link) {
		return nil, "", errors.New("that is not a join link (http://NODE:7777/bootstrap/KEY.tgz)")
	}
	u, _ := url.Parse(link)
	if err := checkNodeURLs([]string{u.Scheme + "://" + u.Host}); err != nil {
		return nil, "", err
	}
	hc := &http.Client{Timeout: 2 * time.Minute}
	resp, err := hc.Get(link)
	if err != nil {
		return nil, "", fmt.Errorf("%s does not answer: %v", u.Host, errors.Unwrap(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("%s answered %s: the link expired or was turned off; ask for a new one", u.Host, resp.Status)
	}
	zr, err := gzip.NewReader(io.LimitReader(resp.Body, joinLinkMax))
	if err != nil {
		return nil, "", errors.New("the link did not give a client bundle")
	}
	tr := tar.NewReader(zr)
	var rawURLs, rawTok string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", errors.New("the client bundle is damaged: " + err.Error())
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		switch path.Base(h.Name) {
		case "urls":
			b, _ := io.ReadAll(io.LimitReader(tr, 64<<10))
			rawURLs = string(b)
		case "token":
			b, _ := io.ReadAll(io.LimitReader(tr, credstore.MaxLen+2))
			rawTok = string(b)
		}
	}
	urls = splitURLs(strings.ReplaceAll(rawURLs, "\n", ","))
	if len(urls) == 0 || rawTok == "" {
		return nil, "", errors.New("the client bundle has no node list or no token")
	}
	if err := checkNodeURLs(urls); err != nil {
		return nil, "", err
	}
	if token, err = credstore.Clean(rawTok); err == nil && len(token) < minTokenLen {
		err = errors.New("the token in the bundle is too short")
	}
	return urls, token, err
}

func (r *startRun) step(title string) { fmt.Fprintf(r.s.Out, "\n== %s\n", title) }

// setup installs the hooks, the MCP server and the instruction blocks for each tool found. A
// machine set up before (state.json) is refreshed without asking, as doctor --fix does.
func (r *startRun) setup(tools string) {
	r.step("2. tools")
	exe, exeErr := executable()
	opts := agentcfg.Options{Tools: splitList(tools), InstallExe: exe, InstallExeErr: exeErr, ClaudeMCP: claudeMCP}
	dry := opts
	dry.DryRun, dry.ClaudeMCP = true, nil
	plan, _ := agentcfg.Setup(dry)
	if !anyChanged(plan) && !anyFailed(plan) {
		r.line("ok", "setup", "hooks, instructions and the binary are current"+toolsNote(plan))
		return
	}
	home, _ := os.UserHomeDir()
	_, setUpBefore := os.Stat(agentcfg.Paths{Home: home}.State())
	switch {
	case setUpBefore == nil || r.yes:
	case r.agent:
		r.line("left", "setup", "the first setup edits the tools' settings, which needs the person's OK")
		r.leave("aivoicebus setup", "install the hooks and instructions (or start --yes)")
		return
	default:
		fmt.Fprintln(r.s.Out, "setup would change:")
		printSteps(r.s.Out, plan, true)
		if !r.confirm("Go ahead?") {
			r.line("left", "setup", "you said no; nothing was changed")
			r.leave("aivoicebus setup", "install the hooks and instructions")
			return
		}
	}
	steps, err := agentcfg.Setup(opts)
	printSteps(r.s.Out, steps, false)
	if err != nil || anyFailed(steps) {
		r.line("FAIL", "setup", "a step failed (above)")
		r.leave("aivoicebus doctor --fix", "repair what setup could not do")
		r.failed = true
		return
	}
	r.line("done", "setup", "hooks, instructions and the binary are in place"+toolsNote(steps))
}

func anyFailed(steps []agentcfg.Step) bool {
	for _, st := range steps {
		if st.Status == agentcfg.Failed {
			return true
		}
	}
	return false
}

// toolsNote names the tools setup found (their instruction step is not skipped).
func toolsNote(steps []agentcfg.Step) string {
	var found []string
	for _, st := range steps {
		if t, ok := strings.CutSuffix(st.Name, " instructions"); ok && st.Status != agentcfg.Skipped {
			found = append(found, t)
		}
	}
	if len(found) == 0 {
		return " (no Claude Code, Codex or Gemini CLI found in your home: run start again after installing one)"
	}
	return " for " + strings.Join(found, ", ")
}

// codexTrust has the person trust the Codex hooks: Codex runs a new hook only once a person
// trusts it in its own screen.
func (r *startRun) codexTrust() {
	r.step("3. Codex hook trust")
	home, _ := os.UserHomeDir()
	hs, err := agentcfg.CodexHooks(home)
	switch {
	case err != nil:
		r.line("WARN", "codex trust", err.Error())
		return
	case len(hs) == 0:
		r.line("skipped", "codex trust", "no aivoicebus hooks in ~/.codex (no Codex here)")
		return
	}
	n := 0
	for _, h := range hs {
		if !h.Trusted {
			n++
		}
	}
	const why = "Codex shows peer messages only once you trust its hooks"
	switch {
	case n == 0:
		r.line("ok", "codex trust", fmt.Sprintf("Codex trusts all %d aivoicebus hooks", len(hs)))
	case r.agent:
		r.refusedForAgent("codex trust", "trusting the Codex hooks", "aivoicebus trust-codex")
	case r.yes:
		r.line("left", "codex trust", fmt.Sprintf("%d hook(s) not trusted yet; trusting them takes a key press in Codex", n))
		r.leave("aivoicebus trust-codex", why)
	case !r.confirm(fmt.Sprintf("%d Codex hook(s) are not trusted yet. Open Codex now to trust them?", n)):
		r.line("left", "codex trust", "you said not now")
		r.leave("aivoicebus trust-codex", why)
	default:
		if err := startTrustCodex(nil, r.s); err != nil {
			r.line("left", "codex trust", "not trusted yet")
			r.leave("aivoicebus trust-codex", why)
			return
		}
		r.line("done", "codex trust", "Codex trusts the hooks")
	}
}

// approver has the person make the key they approve write-mode tasks and token installs with:
// without one such a request waits for an approval nobody can give.
func (r *startRun) approver() {
	r.step("4. approver key")
	const why = "you approve write-mode tasks and token installs with it; without one they wait for ever"
	if _, err := os.Stat(config.ApproverPath()); err == nil {
		if _, err := config.ApproverPublic(); err != nil {
			r.line("WARN", "approver", err.Error())
			return
		}
		r.line("ok", "approver", "a key is here ("+config.ApproverPath()+")")
		return
	}
	switch {
	case r.agent:
		r.refusedForAgent("approver", "choosing the approver passphrase", "aivoicebus approver init")
	case r.yes:
		r.line("left", "approver", "no approver key; its passphrase is yours to type")
		r.leave("aivoicebus approver init", why)
	case !r.confirm("No approver key yet: " + why + ". Make one now (you choose a passphrase)?"):
		r.line("left", "approver", "you said not now")
		r.leave("aivoicebus approver init", why)
	default:
		if err := approverCmd([]string{"init"}, r.s); err != nil {
			r.line("left", "approver", view.OneLine(err.Error(), 120))
			r.leave("aivoicebus approver init", why)
			return
		}
		r.line("done", "approver", "key made, sealed with your passphrase")
	}
}

// notify takes the optional URL a phone alert is posted to (ntfy, a webhook): it is often a
// secret, so it is read without echo and only its host is shown.
func (r *startRun) notify() {
	r.step("5. phone alerts (optional)")
	const later = "aivoicebus watch notify set https://ntfy.sh/YOUR-TOPIC"
	if nu := notifyURL(); nu != "" {
		host := "?"
		if u, err := url.Parse(nu); err == nil {
			host = u.Host
		}
		r.line("ok", "notify", "alerts go to "+host)
		return
	}
	switch {
	case r.agent, r.yes:
		r.line("skipped", "notify", "optional, and the URL is the person's to give; later: "+later)
		return
	}
	got, err := readPassphrase("A phone alert URL (an ntfy topic or a webhook; nothing is shown; Enter skips): ")
	if got = strings.TrimSpace(got); err != nil || got == "" {
		r.line("skipped", "notify", "later: "+later)
		return
	}
	var out bytes.Buffer
	if err := watchNotifyCmd([]string{"set", got}, Streams{Out: &out, Err: r.s.Err}); err != nil {
		r.line("left", "notify", view.OneLine(err.Error(), 120))
		r.leave(later, "a phone alert when a task waits untaken")
		return
	}
	u, _ := url.Parse(got)
	if err := tasks.Notify(got, "aivoicebus: alerts from "+msg.SafeName(identity.Me())+" reach this phone"); err != nil {
		r.line("WARN", "notify", "saved, but the test alert to "+u.Host+" failed: "+view.OneLine(err.Error(), 100))
		return
	}
	r.line("done", "notify", "saved (mode 600); a test alert went to "+u.Host)
}

// doctor runs the full check and shows what is not ok.
func (r *startRun) doctor() {
	r.step("6. doctor")
	var out bytes.Buffer
	args := []string{}
	if !r.meshReady {
		args = append(args, "--offline")
	}
	err := doctor(args, Streams{Out: &out, Err: r.s.Err})
	counts := map[string]int{}
	for _, l := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
		level, _, _ := strings.Cut(l, " ")
		counts[level]++
		if level == "WARN" || level == "FAIL" {
			fmt.Fprintln(r.s.Out, "  "+l)
		}
	}
	detail := fmt.Sprintf("%d ok, %d WARN, %d FAIL (all of it: aivoicebus doctor)", counts["ok"], counts["WARN"], counts["FAIL"])
	if err != nil || counts["FAIL"] > 0 {
		r.line("FAIL", "doctor", detail)
		r.failed = true
		return
	}
	r.line("ok", "doctor", detail)
}

// hello says hello to a session or worker that is listening right now and shows its answer: the
// proof that this machine is on the mesh. With nobody listening it says how to have one listen.
func (r *startRun) hello(wait time.Duration) {
	r.step("7. hello")
	if !r.meshReady {
		r.line("skipped", "hello", "the mesh is not reached yet")
		return
	}
	c, err := sessionClient()
	if err != nil {
		r.line("skipped", "hello", err.Error())
		return
	}
	roster, err := c.Roster()
	if err != nil {
		r.line("WARN", "hello", "the nodes did not list who is there: "+view.OneLine(err.Error(), 100))
		return
	}
	me := identity.Me()
	var listening []msg.RosterEntry
	for _, e := range roster {
		if e.Listening && e.Me != me {
			listening = append(listening, e)
		}
	}
	if len(listening) == 0 {
		r.line("none", "hello", "nobody is listening right now, so there is no one to say hello to")
		fmt.Fprintln(r.s.Out, "  To have a session listen: in Claude Code run /aivoicebus-standby; in Codex or Gemini CLI, the standby prompt;")
		fmt.Fprintln(r.s.Out, "  or keep a worker running: aivoicebus worker install --agent codex. Then run start again to say hello.")
		return
	}
	sort.Slice(listening, func(i, j int) bool { return listening[i].Seen.After(listening[j].Seen) })
	to := listening[0].Me
	name := view.Name(to, msg.MaxIdentity)
	if r.ask && !r.confirm(name+" is listening. Say hello and wait for its answer?") {
		r.line("skipped", "hello", "you said not now")
		return
	}
	nonce := make([]byte, 3)
	_, _ = rand.Read(nonce)
	text := fmt.Sprintf("hello from %s: a new machine (aivoicebus start) checking that it reaches you. Please answer with one line, e.g. \"hello back\". (start %s)", msg.SafeName(me), hex.EncodeToString(nonce))
	args := []string{"--to", to, "--ttl", "1h", "--wait-reply", wait.String()}
	if _, agent, _ := strings.Cut(to, "/"); agent == "worker" || strings.HasSuffix(agent, ".worker") {
		args = append(args, "--kind", msg.KindTask) // a worker answers tasks, and only from the machines it allows
	}
	err = sendWith(append(args, text), r.s, false)
	var ee exitError
	switch {
	case err == nil:
		r.line("ok", "hello", name+" answered (above): this machine is on the mesh")
	case errors.As(err, &ee) && ee.code == exitTimeout:
		r.line("no reply", "hello", "the hello went out, and "+name+" did not answer within "+wait.String()+" (it may be asking its person first, or a worker that does not allow this machine)")
	default:
		r.line("WARN", "hello", view.OneLine(err.Error(), 120))
	}
}

func (r *startRun) summary(took time.Duration) {
	fmt.Fprintln(r.s.Out)
	switch {
	case r.failed:
		fmt.Fprintf(r.s.Out, "start: not finished (%s): a step FAILED above.\n", took.Round(time.Second))
	default:
		fmt.Fprintf(r.s.Out, "start: done in %s.\n", took.Round(time.Second))
	}
	if len(r.left) > 0 {
		fmt.Fprintln(r.s.Out, "Left for you (in your own terminal):")
		for _, l := range r.left {
			fmt.Fprintf(r.s.Out, "  %-28s %s\n", l[0], l[1])
		}
	}
	if r.failed {
		return
	}
	fmt.Fprintln(r.s.Out, "What you can do now:")
	fmt.Fprintln(r.s.Out, "  aivoicebus who                                   who is on the mesh")
	fmt.Fprintln(r.s.Out, "  aivoicebus task MACHINE \"...\" --wait-reply 10m   give a machine a task and wait for the answer")
	fmt.Fprintln(r.s.Out, "  aivoicebus inbox                                 read what was sent to you")
}
