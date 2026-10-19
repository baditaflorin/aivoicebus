package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/credstore"
	"github.com/baditaflorin/aivoicebus/internal/identity"
)

// joinRunsPath is where join keeps the times it sent a probe (the same six an hour as selftest, so
// a script looping join cannot fill the log with probes); a variable so tests can point elsewhere.
var joinRunsPath = func() string { return filepath.Join(config.Dir(), "join-runs") }

// minTokenLen is what doctor already demands of the token file.
const minTokenLen = 24

// join brings a new machine onto the mesh with one command: it checks where the token is going,
// takes the token (from a pipe or a prompt that does not echo, never from an argument), proves it
// against the nodes IN MEMORY, and only then writes the token and runs setup (which also installs
// this binary). The proof is a send/read/retract round trip. A failure at any point leaves the
// files this machine already had exactly as they were.
func join(args []string, s Streams) error {
	fs := newFlagSet("join")
	urls := fs.String("url", "", "node URLs (comma-separated); default: the ones already configured here")
	tools := fs.String("tools", "", "tools to configure: claude,codex,gemini (default: all that are installed)")
	replace := fs.Bool("replace-token", false, "ask for the token again even if one is saved (when the mesh refuses it)")
	trust := fs.Bool("trust-url", false, "allow node URLs this machine has no signed node list for; you will be asked to type each host:port to confirm")
	dry := fs.Bool("dry-run", false, "show what would be done; write and send nothing")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: aivoicebus join [--url URLS] [--trust-url] [--tools LIST] [--replace-token] [--dry-run]  (the token is asked for, or piped in: pbpaste | aivoicebus join --trust-url --url ...; a node this machine has no signed list for is confirmed by typing its host:port at a terminal)")
	}
	return joinRun(joinOpts{urls: *urls, tools: *tools, replace: *replace, trust: *trust, dry: *dry}, s)
}

// joinOpts is what join does; start fills it from what the person typed at its prompts.
type joinOpts struct {
	urls, tools         string
	replace, trust, dry bool
	// token is a token already in hand (start took it from a join link or a pipe); empty asks for
	// it. tokenFrom says where it came from.
	token, tokenFrom string
	// typed says the person typed or pasted the URLs (or the join link that holds them) at this
	// terminal a moment ago: that is the confirmation join otherwise asks for, so it is not asked
	// twice. nodesFrom says how they came.
	typed     bool
	nodesFrom string
}

func joinRun(o joinOpts, s Streams) error {
	line := func(status, name, detail string) { fmt.Fprintf(s.Out, "%-12s %-22s %s\n", status, name, detail) }
	fail := func(stage string, err error) error {
		fmt.Fprintf(s.Out, "\njoin FAILED at %s: %v\n", stage, err)
		return exitError{1}
	}

	old := configuredURLs()
	nodeURLs := splitURLs(o.urls)
	if len(nodeURLs) == 0 {
		nodeURLs = old
	}
	if len(nodeURLs) == 0 {
		return errors.New("no node URL: pass --url http://node1:7777,http://node2:7777")
	}
	if err := checkNodeURLs(nodeURLs); err != nil {
		return err
	}
	confirm, err := needConfirmation(nodeURLs, old, o.trust || o.typed, s.Err)
	if err != nil {
		return err
	}
	if o.typed {
		confirm = nil
	}

	// 1. who gets the token: confirmed by typing, before the token is even asked for
	switch {
	case len(confirm) == 0 && o.typed:
		line("ok", "nodes", strings.Join(nodeURLs, ",")+" ("+o.nodesFrom+")")
	case len(confirm) == 0:
		line("ok", "nodes", strings.Join(nodeURLs, ","))
	case o.dry:
		line("would ask", "nodes", "you to type "+strings.Join(confirm, ", ")+" to confirm the token may be sent there")
	default:
		for _, h := range confirm {
			ok, err := confirmHost(fmt.Sprintf("The mesh token will be sent to %s. Type it exactly as shown (host:port) to confirm: ", h), h)
			if err != nil {
				return fail("nodes", err)
			}
			if !ok {
				return fail("nodes", fmt.Errorf("%s was not confirmed; nothing was sent or written", h))
			}
		}
		line("ok", "nodes", strings.Join(nodeURLs, ",")+" (confirmed)")
	}

	// 2. the candidate token, in memory only
	tokenPath := config.TokenPath("")
	saved, haveSaved := readSavedToken(tokenPath)
	var tok string
	switch {
	case o.token != "":
		tok = o.token
		line("ok", "token", o.tokenFrom+" (shown nowhere)")
	case haveSaved && !o.replace:
		tok = saved
		line("ok", "token", tokenPath+" (kept; --replace-token asks again)")
	case o.dry:
		line("would ask", "token", "from a pipe or a prompt, saved to "+tokenPath+" (mode 600) once the mesh accepts it")
	default:
		raw, err := readSecret(s, "Mesh token (paste it; nothing is shown): ", "pbpaste | aivoicebus join --trust-url --url "+strings.Join(nodeURLs, ","))
		if err != nil {
			return fail("token", err)
		}
		if tok, err = credstore.Clean(raw); err == nil && len(tok) < minTokenLen {
			err = fmt.Errorf("that is shorter than %d characters: was the wrong thing pasted?", minTokenLen)
		}
		if err != nil {
			return fail("token", err)
		}
	}

	// 3. prove it against the nodes before anything is written
	if !o.dry {
		c := &client.Client{URLs: nodeURLs, Token: tok}
		if _, err := meshProbeWith(c); err != nil {
			if errors.Is(err, agentcfg.ErrTokenRejected) {
				return fail("probe", fmt.Errorf("%v; ask whoever runs the mesh for the current token, then run `aivoicebus join --replace-token` (nothing on this machine was changed)", err))
			}
			return fail("probe", fmt.Errorf("%v; check the node URLs and the network, then run `aivoicebus join` again (nothing on this machine was changed)", err))
		}
		line("ok", "mesh", "reachable, token accepted")
		if err := probeAllowed(joinRunsPath(), time.Now()); err != nil {
			return fail("probe", fmt.Errorf("%v (nothing on this machine was changed)", err))
		}
		res, err := probeRoundTrip(c)
		if err != nil {
			return fail("probe", fmt.Errorf("%v (nothing on this machine was changed)", err))
		}
		line("ok", "probe", fmt.Sprintf("sent in %s, read back in %s", res.sent.Round(time.Millisecond), res.read.Round(time.Millisecond)))
		for _, w := range res.warnings {
			line("WARN", "probe", w)
		}
		if res.clean() {
			line("ok", "probe cleanup", "acknowledged and retracted")
		}
		if o.token != "" || !(haveSaved && !o.replace) { // a kept token is already on disk
			if err := writeTokenFile(tokenPath, tok); err != nil {
				return fail("token", err)
			}
			line("changed", "token", tokenPath+" (mode 600)")
		}
		if os.Getenv("AIVOICEBUS_TOKEN") != "" {
			line("WARN", "token", "AIVOICEBUS_TOKEN is set in this shell and takes precedence over the saved file")
		}
	}

	// 4. install + env + hooks + instruction blocks: the same path `setup` and `doctor --fix` use
	var list []string
	for _, t := range strings.Split(o.tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			list = append(list, t)
		}
	}
	exe, exeErr := executable()
	steps, err := agentcfg.Setup(agentcfg.Options{URL: strings.Join(nodeURLs, ","), Tools: list, DryRun: o.dry, InstallExe: exe, InstallExeErr: exeErr, ClaudeMCP: claudeMCP})
	printSteps(s.Out, steps, o.dry)
	if err != nil {
		return fail("setup", err)
	}
	if o.dry {
		line("would", "probe", "send a short-lived note to a throwaway identity on this machine, read it back, and retract it")
		fmt.Fprintln(s.Out, "\njoin dry run: nothing was written or sent")
		return nil
	}
	fmt.Fprintln(s.Out, "\njoin ok: this machine can send and read. Open a new shell (or `. ~/.config/aivoicebus/env`) for PATH; `aivoicebus doctor` shows the full picture.")
	return nil
}

func splitURLs(list string) []string {
	var out []string
	for _, u := range strings.Split(list, ",") {
		if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// configuredURLs is what this machine's environment (and env file) already names as nodes.
func configuredURLs() []string { return splitURLs(os.Getenv("AIVOICEBUS_URL")) }

// lookupIP resolves a node's host name; a variable so tests need no DNS.
var lookupIP = net.LookupIP

// checkNodeURLs refuses what cannot be a node address, and plain http to anything that is not
// loopback, a private network or the tailnet (100.64.0.0/10): the token would cross the open internet.
func checkNodeURLs(list []string) error {
	if err := config.SafeNodeURLs(strings.Join(list, ",")); err != nil {
		return fmt.Errorf("%v: it is written to a file the shell reads", err)
	}
	for _, raw := range list {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
			return fmt.Errorf("%q is not a node URL (expected http://host:port)", identity.Printable(raw))
		}
		if u.Scheme == "http" && !privateHost(u.Hostname()) {
			return fmt.Errorf("%q is plain http to a host outside private and tailnet ranges: the token would cross the open internet; use https", identity.Printable(raw))
		}
	}
	return nil
}

func privateHost(host string) bool {
	ips := []net.IP{net.ParseIP(host)}
	if ips[0] == nil {
		var err error
		if ips, err = lookupIP(host); err != nil || len(ips) == 0 {
			return false
		}
	}
	_, tailnet, _ := net.ParseCIDR("100.64.0.0/10")
	for _, ip := range ips {
		if !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || tailnet.Contains(ip)) {
			return false
		}
	}
	return true
}

// needConfirmation says which hosts the person must type before the token may go there: every
// URL the signed node list does not vouch for (which also needs --trust-url), and, when this
// machine is already configured for other nodes, every URL that is new. It prints the old-versus-new
// warning itself.
func needConfirmation(urls, old []string, trust bool, w io.Writer) ([]string, error) {
	d := config.LoadDiscovered()
	known := map[string]bool{}
	if !d.Expired(time.Now()) {
		for _, n := range d.Nodes {
			known[strings.TrimRight(n, "/")] = true
		}
	}
	was := map[string]bool{}
	for _, u := range old {
		was[u] = true
	}
	var untrusted, fresh []string
	for _, u := range urls {
		if !known[u] {
			untrusted = append(untrusted, u)
		}
		if len(old) > 0 && !was[u] {
			fresh = append(fresh, u)
		}
	}
	if len(untrusted) > 0 && !trust {
		return nil, fmt.Errorf("%s is not in this machine's signed node list, and the token must not go to an address nobody vouched for; if it really is your node, repeat with --trust-url (you will be asked to type its host:port)", strings.Join(untrusted, ", "))
	}
	if len(fresh) > 0 {
		fmt.Fprintf(w, "warning: this machine is configured for %s; join would send the token to %s instead\n", strings.Join(old, ","), strings.Join(urls, ","))
	}
	var hosts []string
	seen := map[string]bool{}
	for _, u := range append(untrusted, fresh...) {
		if p, err := url.Parse(u); err == nil && !seen[p.Host] {
			seen[p.Host] = true
			hosts = append(hosts, p.Host)
		}
	}
	return hosts, nil
}

// confirmHost asks on the terminal, not stdin (which may carry the token), for want to be typed.
// A variable so tests can answer.
var confirmHost = func(prompt, want string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, errors.New("confirming a new node address needs a terminal to type on, and there is none here; run join from an interactive shell")
	}
	defer tty.Close()
	fmt.Fprint(tty, prompt)
	got, _ := bufio.NewReader(tty).ReadString('\n')
	return strings.TrimSpace(got) == want, nil
}

// readSavedToken returns the token in the file the client really reads, when it is usable.
func readSavedToken(path string) (string, bool) {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	tok, err := credstore.Clean(string(b))
	return tok, err == nil && len(tok) >= minTokenLen
}

// writeTokenFile saves the token privately: a file (CreateTemp makes it 0600) renamed
// into place, so it is never readable by others and a failed write never leaves half a token
// behind or loses the one that was there.
func writeTokenFile(path, tok string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(tok + "\n")
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	return werr
}
