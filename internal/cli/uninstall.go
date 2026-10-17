package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/agentcfg"
)

// uninstallCmd removes what setup added to this machine.
func uninstallCmd(args []string, s Streams) error {
	fs := newFlagSet("uninstall")
	tools := fs.String("tools", "", "tools to clean: claude,codex,gemini (default: all)")
	dry := fs.Bool("dry-run", false, "show what would be removed; change nothing")
	purge := fs.Bool("purge", false, "also delete ~/.config/aivoicebus (token, saved credentials, sent-message log, trusted keys) and the installed binary")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
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
	opts := agentcfg.RemoveOptions{Tools: list, DryRun: true, Purge: *purge, ClaudeMCPRemove: claudeMCPRemove}
	plan, err := agentcfg.Uninstall(opts)
	if err != nil && !*dry {
		printSteps(s.Out, plan, true)
		return err // something it cannot vouch for: nothing is changed until that is dealt with
	}
	if *dry {
		if *asJSON {
			return writeSteps(s.Out, plan)
		}
		printSteps(s.Out, plan, true)
		return err
	}
	if !anyChanged(plan) {
		if *asJSON {
			return writeSteps(s.Out, plan)
		}
		printSteps(s.Out, plan, false)
		fmt.Fprintln(s.Out, "nothing to remove")
		return nil
	}
	if !*yes {
		if !*asJSON {
			printSteps(s.Out, plan, true)
		}
		ok, err := askYesNo(s, "Remove these? [y/N] ")
		switch {
		case errors.Is(err, errNoTerminal):
			return errors.New("uninstall changes files in your home directory and there is no terminal to confirm on: run it in a terminal, or pass --yes (--dry-run shows the plan)")
		case err != nil:
			return err
		case !ok:
			return errors.New("cancelled: nothing was changed")
		}
	}
	opts.DryRun = false
	steps, err := agentcfg.Uninstall(opts)
	if *asJSON {
		if err != nil {
			return err
		}
		return writeSteps(s.Out, steps)
	}
	printSteps(s.Out, steps, false)
	if err == nil {
		fmt.Fprintln(s.Out, "Not touched: an MCP server you registered yourself (claude mcp remove aivoicebus) and any message data on a node you run.")
		if !*purge {
			fmt.Fprintln(s.Out, "The binary stays at ~/.local/bin/aivoicebus; --purge removes it.")
		}
	}
	return err
}

func anyChanged(steps []agentcfg.Step) bool {
	for _, st := range steps {
		if st.Status == agentcfg.Changed {
			return true
		}
	}
	return false
}
