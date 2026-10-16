package cli

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// maxGroupNames is how many sessions a `who --tools` line names before it counts the rest.
const maxGroupNames = 5

// toolOf is the tool a session runs: its declared tool label, else the tool part of its
// identity (an old node reports no labels), else "unknown".
func toolOf(e msg.RosterEntry) string {
	if t := e.Labels["tool"]; t != "" {
		return t
	}
	_, agent, _ := strings.Cut(e.Me, "/")
	if t, _, _ := strings.Cut(agent, "."); t != "" {
		return t
	}
	return "unknown"
}

// printToolGroups writes sessions grouped by tool, then by repo, so "who is working on what"
// reads at a glance. Everything shown came from peers, so it goes through view.Name.
func printToolGroups(w io.Writer, rows []msg.RosterEntry) {
	byTool := map[string]map[string][]string{} // tool -> repo -> sessions, each with the caps it advertises
	count := map[string]int{}
	for _, e := range rows {
		tool, repo := toolOf(e), e.Labels["repo"]
		if repo == "" {
			repo = "(no repo)"
		}
		if byTool[tool] == nil {
			byTool[tool] = map[string][]string{}
		}
		name := view.Name(e.Me, 48)
		if caps := e.Labels[msg.CapsLabel]; caps != "" {
			name += " [" + view.Name(caps, 64) + "]"
		}
		byTool[tool][repo] = append(byTool[tool][repo], name)
		count[tool]++
	}
	for _, tool := range slices.Sorted(maps.Keys(byTool)) {
		fmt.Fprintf(w, "%s (%d)\n", view.Name(tool, 32), count[tool])
		repos := slices.Sorted(maps.Keys(byTool[tool]))
		slices.SortStableFunc(repos, func(a, b string) int { return len(byTool[tool][b]) - len(byTool[tool][a]) })
		for _, r := range repos {
			names := byTool[tool][r]
			slices.Sort(names)
			shown := names[:min(len(names), maxGroupNames)]
			line := strings.Join(shown, ", ")
			if more := len(names) - len(shown); more > 0 {
				line += fmt.Sprintf(", +%d more", more)
			}
			fmt.Fprintf(w, "  %-28s %d  %s\n", view.Name(r, 28), len(names), line)
		}
	}
}
