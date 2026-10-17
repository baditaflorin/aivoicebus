package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// codexBundled is where the ChatGPT desktop app keeps its Codex CLI when none is on PATH.
const codexBundled = "/Applications/ChatGPT.app/Contents/Resources/codex-cli/bin/codex"

// trustCodexCmd walks the user through trusting the hooks setup put in ~/.codex/hooks.json:
// Codex runs a new hook only after a person trusts it in its own screen. It shows the hooks, starts
// Codex on this terminal (Codex fails on redirected input or output, and its keys stay the user's:
// trusting is their decision), and afterwards reads ~/.codex/config.toml to say whether it worked.
func trustCodexCmd(args []string, s Streams) error {
	fs := newFlagSet("trust-codex")
	check := fs.Bool("check", false, "only say whether Codex trusts the hooks; start nothing")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return outbox.Usagef("usage: trust-codex [--check]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	hs, err := agentcfg.CodexHooks(home)
	if err != nil {
		return err
	}
	if len(hs) == 0 {
		return errors.New("no aivoicebus hooks in ~/.codex/hooks.json: run `aivoicebus setup` first (it adds them when ~/.codex exists)")
	}
	untrusted := 0
	for _, h := range hs {
		state := "trusted"
		if !h.Trusted {
			state, untrusted = "NOT trusted", untrusted+1
		}
		fmt.Fprintf(s.Out, "  %-17s %s\n", h.Event, state)
	}
	if untrusted == 0 {
		fmt.Fprintln(s.Out, "Codex trusts every aivoicebus hook; nothing to do.")
		return nil
	}
	if *check {
		fmt.Fprintf(s.Err, "%d hook(s) not trusted yet: run `aivoicebus trust-codex`\n", untrusted)
		return exitError{exitFailure}
	}
	if fi, err := os.Stdin.Stat(); err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return errors.New("trust-codex starts Codex, which needs a terminal: run it in one, not from a pipe or a script")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		if _, e := os.Stat(codexBundled); e != nil {
			return errors.New("no codex on PATH (or in ChatGPT.app): install Codex first")
		}
		codex = codexBundled
	}
	fmt.Fprintln(s.Out, "\nThe hooks run `aivoicebus hook --tool codex` at session start and on each prompt, which shows")
	fmt.Fprintln(s.Out, "unread peer messages to Codex. Codex now starts; on its screens:")
	fmt.Fprintln(s.Out, "  \"Background server has incompatible feature settings\": pick 1 (run without daemon this time)")
	fmt.Fprintln(s.Out, "  \"Hooks need review\": press 2 (Trust all and continue), then Enter")
	fmt.Fprintln(s.Out, "  then type /quit and Enter")
	fmt.Fprintln(s.Out, "\"Trust all\" trusts every hook Codex lists as new or changed, not only aivoicebus's.")
	fmt.Fprint(s.Out, "Press Enter to start Codex (Ctrl-C to stop here)... ")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		return errors.New("stopped; nothing was started")
	}
	cmd := exec.Command(codex)
	cmd.Dir = home
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	_ = cmd.Run() // its exit status says nothing about trust; config.toml does
	hs, err = agentcfg.CodexHooks(home)
	if err != nil {
		return err
	}
	var still []string
	for _, h := range hs {
		if !h.Trusted {
			still = append(still, h.Event)
		}
	}
	if len(still) > 0 {
		fmt.Fprintln(s.Err, "\nCodex did not save trust for "+strings.Join(still, ", ")+": run `aivoicebus trust-codex` again and press 2 on \"Hooks need review\"")
		return exitError{exitFailure}
	}
	fmt.Fprintf(s.Out, "\nok: Codex trusts all %d aivoicebus hooks (~/.codex/config.toml). New Codex sessions show peer messages.\n", len(hs))
	return nil
}
