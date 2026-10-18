package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/assets"
	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/release"
	"github.com/baditaflorin/aivoicebus/internal/version"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// executable is the running binary; a variable so tests can point it at a stand-in.
var executable = os.Executable

func versionCmd(args []string, s Streams) error {
	fs := newFlagSet("version")
	asJSON := fs.Bool("json", false, "print the version and release as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, struct {
			Version string `json:"version"`
			Release string `json:"release"`
		}{version.String(), version.Release})
	}
	fmt.Fprintln(s.Out, version.String())
	return nil
}

func usageCmd(_ []string, s Streams) error {
	fmt.Fprint(s.Out, assets.Usage())
	return nil
}

// setup brings this machine's aivoicebus configuration in line with this binary.
func setup(args []string, s Streams) error {
	fs := newFlagSet("setup")
	url := fs.String("url", "", "node URLs (comma-separated) to write to the env file")
	tools := fs.String("tools", "", "tools to configure: claude,codex,gemini (default: all that are installed)")
	dry := fs.Bool("dry-run", false, "show what would change; write nothing")
	yes := fs.Bool("yes", false, "do not ask before the first setup changes files outside ~/.config/aivoicebus (the tools' settings and instruction files, ~/.zshrc)")
	asJSON := fs.Bool("json", false, "print the steps as JSON")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	var list []string
	for _, t := range strings.Split(*tools, ",") {
		if t = strings.TrimSpace(t); t != "" {
			list = append(list, t)
		}
	}
	if *url != "" { // the same rule join applies: a token must not be pointed at plain http on the open internet
		if err := checkNodeURLs(splitList(*url)); err != nil {
			return err
		}
	}
	if !*dry && !*yes {
		if err := confirmFirstSetup(s, *url, list, *asJSON); err != nil {
			return err
		}
	}
	// The hooks run ~/.local/bin/aivoicebus, so put this binary there when nothing usable is (as
	// join and doctor --fix do); an installed one is left alone: updating it is `update`'s job.
	exe, exeErr := executable()
	steps, err := agentcfg.Setup(agentcfg.Options{URL: *url, Tools: list, DryRun: *dry, InstallExe: exe, InstallExeErr: exeErr, ClaudeMCP: claudeMCP})
	if *asJSON {
		if err != nil {
			return err // a failure is the error object on stderr and nothing on stdout
		}
		return writeSteps(s.Out, steps)
	}
	printSteps(s.Out, steps, *dry)
	return err
}

// confirmFirstSetup asks before the first setup on a machine edits files that are not
// aivoicebus's own (Claude Code's settings, the tools' instruction files, ~/.zshrc). Once state.json
// exists the machine has been set up with consent, and refreshing it (update, doctor --fix) does not ask again.
func confirmFirstSetup(s Streams, url string, tools []string, asJSON bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if _, err := os.Stat(agentcfg.Paths{Home: home}.State()); err == nil {
		return nil
	}
	exe, exeErr := executable()
	plan, _ := agentcfg.Setup(agentcfg.Options{URL: url, Tools: tools, DryRun: true, InstallExe: exe, InstallExeErr: exeErr})
	if !anyChanged(plan) {
		return nil
	}
	if !asJSON {
		fmt.Fprintln(s.Out, "setup would change:")
		printSteps(s.Out, plan, true)
	}
	ok, err := askYesNo(s, "Go ahead? [y/N] ")
	switch {
	case errors.Is(err, errNoTerminal):
		return errors.New("setup edits files in your home directory (see `setup --dry-run`) and there is no terminal to confirm on: run it in a terminal, or pass --yes")
	case err != nil:
		return err
	case !ok:
		return errors.New("cancelled: nothing was changed")
	}
	return nil
}

// stepView and checkView are the --json shapes of setup's steps and doctor's checks.
type stepView struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}
type checkView struct {
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

// writeSteps prints steps as the JSON array setup --json documents.
func writeSteps(w io.Writer, steps []agentcfg.Step) error {
	views := make([]stepView, len(steps))
	for i, st := range steps {
		views[i] = stepView{st.Name, st.Status, st.Detail}
	}
	return writeJSON(w, views)
}

func printSteps(w io.Writer, steps []agentcfg.Step, dry bool) {
	for _, st := range steps {
		status := st.Status
		if dry && status == agentcfg.Changed {
			status = "would change"
		}
		fmt.Fprintf(w, "%-12s %-22s %s\n", status, st.Name, st.Detail)
	}
}

// doctor reports how this machine compares with what this binary sets up.
func doctor(args []string, s Streams) error {
	fs := newFlagSet("doctor")
	fix := fs.Bool("fix", false, "run setup first, to repair what is out of date (also installs this binary if none is installed)")
	dry := fs.Bool("dry-run", false, "with --fix: show what would be repaired; write nothing")
	offline := fs.Bool("offline", false, "skip the mesh reachability check")
	asJSON := fs.Bool("json", false, "print the checks as JSON (level ok, WARN or FAIL); exit 1 on any FAIL")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *dry && !*fix {
		return errors.New("--dry-run goes with --fix")
	}
	if *fix {
		exe, exeErr := executable()
		steps, err := agentcfg.Setup(agentcfg.Options{DryRun: *dry, InstallExe: exe, InstallExeErr: exeErr, ClaudeMCP: claudeMCP})
		if *dry { // the checks below describe the machine as it is, which is not what the plan is about
			if *asJSON {
				if err != nil {
					return err
				}
				return writeSteps(s.Out, steps)
			}
			printSteps(s.Out, steps, true)
			return err
		}
		if !*asJSON {
			printSteps(s.Out, steps, false)
			fmt.Fprintln(s.Out)
		}
		if err != nil {
			return err
		}
	}
	opts := agentcfg.DoctorOptions{Identity: identity.Printable(identity.Me() + "  (" + identity.Explain().Why() + ")")}
	latest := ""
	if !*offline {
		opts.Mesh = func() error {
			var err error
			latest, err = meshProbe()
			return err
		}
	}
	checks := agentcfg.Doctor(opts)
	if latest != "" {
		checks = append(checks, releaseCheck(latest))
	}
	if !*offline {
		if c, ok := featureCheck(); ok {
			checks = append(checks, c)
		}
		if c, ok := tokenKindCheck(); ok {
			checks = append(checks, c)
		}
	}
	var views []checkView
	for _, c := range checks {
		level := map[string]string{agentcfg.LevelOK: "ok", agentcfg.LevelWarn: "WARN", agentcfg.LevelFail: "FAIL"}[c.Level]
		if *asJSON {
			views = append(views, checkView{c.Name, level, c.Detail})
			continue
		}
		fmt.Fprintf(s.Out, "%-5s %-22s %s\n", level, c.Name, c.Detail)
	}
	if *asJSON {
		if err := writeJSON(s.Out, append([]checkView{}, views...)); err != nil {
			return err
		}
	}
	if agentcfg.HasFailures(checks) {
		return exitError{1}
	}
	return nil
}

// meshProbe asks the nodes whether they accept this machine's token and returns the newest
// release they announced. A node that refuses the token is told apart from nodes that do not
// answer (the error wraps agentcfg.ErrTokenRejected): the first leaves the machine deaf until
// someone acts, the second may just be a network that is away.
func meshProbe() (string, error) {
	c, err := client.FromEnv()
	if err != nil {
		return "", err
	}
	return meshProbeWith(c)
}

// meshProbeWith is meshProbe for a client built by the caller (join proves a token it has not saved yet).
func meshProbeWith(c *client.Client) (string, error) {
	c.Timeout = 3 * time.Second
	_, err := c.TokenInfo()
	if client.IsNotFound(err) { // a node from before /v1/token: the roster says as much
		_, err = c.Who()
	}
	switch {
	case client.IsRejected(err):
		return "", fmt.Errorf("%w: %v", agentcfg.ErrTokenRejected, err)
	case err != nil:
		return "", err
	}
	return c.Latest, nil
}

// releaseCheck compares the release the mesh announced with the running one.
func releaseCheck(latest string) agentcfg.Check {
	if release.Newer(latest, version.Release) {
		return agentcfg.Check{Name: "release", Level: agentcfg.LevelWarn,
			Detail: fmt.Sprintf("release %s is available (this is %s); `aivoicebus update` installs it", latest, version.Release)}
	}
	return agentcfg.Check{Name: "release", Level: agentcfg.LevelOK, Detail: "up to date (the mesh announces " + latest + ")"}
}

// featureCheck says what each configured node advertises (msg.Health.Features), so one node that says
// nothing first in the list does not hide one that does. A node that says nothing or does not
// answer is left out: that is not a fault of this machine; with no feature advertised anywhere there
// is no line at all.
func featureCheck() (agentcfg.Check, bool) {
	c, err := client.FromEnv()
	if err != nil {
		return agentcfg.Check{}, false
	}
	c.Timeout = 3 * time.Second
	var parts []string
	for _, u := range c.URLs { // each configured node by itself: the first one may be the old one
		one := &client.Client{URLs: []string{u}, Token: c.Token, Timeout: c.Timeout}
		h, err := one.Health()
		if err != nil || len(h.Features) == 0 {
			continue
		}
		parts = append(parts, view.OneLine(h.Node, 30)+": "+featureList(&h))
	}
	if len(parts) == 0 {
		return agentcfg.Check{}, false
	}
	return agentcfg.Check{Name: "node features", Level: agentcfg.LevelOK, Detail: strings.Join(parts, "; ")}, true
}

// splitList splits a comma-separated list, dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// tokenKindCheck says whether this machine still uses the mesh's shared token, with which anyone
// holding it can send as this machine; a machine token binds its messages to its name.
func tokenKindCheck() (agentcfg.Check, bool) {
	c, err := client.FromEnv()
	if err != nil {
		return agentcfg.Check{}, false
	}
	kind, host, err := c.TokenKind()
	switch {
	case err != nil || kind == "":
		return agentcfg.Check{}, false // a node too old to say, or none answering: the mesh line says so
	case kind == "machine":
		return agentcfg.Check{Name: "token kind", Level: agentcfg.LevelOK, Detail: "a machine token, bound to " + host}, true
	case kind == "observer":
		return agentcfg.Check{Name: "token kind", Level: agentcfg.LevelWarn, Detail: "the observer token: read-only, this machine cannot send"}, true
	}
	return agentcfg.Check{Name: "token kind", Level: agentcfg.LevelWarn, Detail: "the mesh's shared token: whoever holds it can send as this machine. The operator gives it its own: deploy/machine-token.sh add " + msg.HostOf(identity.Me()) + " --apply"}, true
}
