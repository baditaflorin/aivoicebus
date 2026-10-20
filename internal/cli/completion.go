package cli

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// completionSubs are the subcommands a command takes as its first word.
var completionSubs = map[string][]string{
	"token":     {"hand", "install", "report"},
	"fleet":     {"update"},
	"schedule":  {"add", "list", "rm", "run"},
	"watch":     {"notify"},
	"worker":    {"allow", "install", "uninstall", "status"},
	"secret":    {"save", "list", "rm", "curl", "path"},
	"lease":     {"take", "release", "list"},
	"room":      {"join", "leave", "invite", "say", "wait", "who"},
	"approver":  {"init", "show"},
	"trust-key": {"add", "list", "rm"},
}

// completionSubSubs are the words after a subcommand, where there are fixed ones.
var completionSubSubs = map[string][]string{
	"watch notify": {"set", "clear", "test"},
}

var reFlagLine = regexp.MustCompile(`^\s+-{1,2}([a-z][a-z0-9-]*)`)

// reUsageFlag finds the flags a hand-parsed command names in its usage line ("[--person] [--for 1h]").
var reUsageFlag = regexp.MustCompile(`(?:^|[\s\[|])--([a-z][a-z0-9-]*)`)

// completeCmd is `aivoicebus __complete WORD...`: the candidates for the last word, one per line,
// for the shell scripts `aivoicebus completion` prints. It offers the commands `help` lists, their
// subcommands, and their flags, read from the command's own -h output (which stops at the flags,
// so nothing is run): the completion never goes stale as flags change.
func completeCmd(args []string, s Streams) error {
	if len(args) == 0 {
		args = []string{""}
	}
	cur := args[len(args)-1]
	words := args[:len(args)-1]
	var out []string
	switch {
	case len(words) == 0:
		for _, t := range helpTopics {
			if !strings.HasPrefix(t.name, "__") {
				out = append(out, t.name)
			}
		}
	case strings.HasPrefix(cur, "-"):
		path := words[:1]
		if subs := completionSubs[words[0]]; len(words) > 1 && contains(subs, words[1]) {
			path = words[:2]
		}
		out = flagsOf(path)
	case len(words) == 1 && words[0] == "help":
		for _, t := range helpTopics {
			out = append(out, t.name)
		}
	case len(words) == 1:
		out = completionSubs[words[0]]
	case len(words) == 2:
		out = completionSubSubs[words[0]+" "+words[1]]
	}
	sort.Strings(out)
	for _, c := range out {
		if strings.HasPrefix(c, cur) {
			fmt.Fprintln(s.Out, c)
		}
	}
	return nil
}

// flagsOf runs `aivoicebus PATH... -h` in this process with no input and keeps the flag names. Only
// the commands `help` lists qualify (a test proves -h is harmless for each of them).
func flagsOf(path []string) []string {
	if !listed(path[0]) || path[0] == "hook" || path[0] == "mcp" { // internal: run by the tools, not by people
		return nil
	}
	fn, ok := commands[path[0]]
	if !ok {
		return nil
	}
	var buf bytes.Buffer
	if err := fn(append(append([]string{}, path[1:]...), "-h"), Streams{In: strings.NewReader(""), Out: &buf, Err: &buf}); err != nil {
		fmt.Fprintln(&buf, err) // a hand-parsed command answers -h with its usage as the error
	}
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, "--"+name)
		}
	}
	for _, l := range strings.Split(buf.String(), "\n") {
		if m := reFlagLine.FindStringSubmatch(l); m != nil {
			add(m[1])
		}
		if strings.Contains(l, "usage:") {
			for _, m := range reUsageFlag.FindAllStringSubmatch(l, -1) {
				add(m[1])
			}
		}
	}
	return out
}

func listed(name string) bool {
	for _, t := range helpTopics {
		if t.name == name {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// completionCmd prints the completion script for a shell.
func completionCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: completion zsh|bash|fish   (zsh: aivoicebus completion zsh > \"${fpath[1]}/_aivoicebus\", then start a new shell)")
	if len(args) != 1 {
		return usage
	}
	switch args[0] {
	case "zsh":
		fmt.Fprint(s.Out, zshCompletion)
	case "bash":
		fmt.Fprint(s.Out, bashCompletion)
	case "fish":
		fmt.Fprint(s.Out, fishCompletion)
	default:
		return usage
	}
	return nil
}

const zshCompletion = `#compdef aivoicebus
# aivoicebus completion for zsh: aivoicebus completion zsh > "${fpath[1]}/_aivoicebus", then a new shell.
_aivoicebus() {
  local -a cands
  cands=("${(@f)$(aivoicebus __complete "${words[@]:1:$((CURRENT-1))}" 2>/dev/null)}")
  compadd -- ${cands:#}
}
compdef _aivoicebus aivoicebus
`

const bashCompletion = `# aivoicebus completion for bash: aivoicebus completion bash > ~/.aivoicebus-completion.bash,
# then add "source ~/.aivoicebus-completion.bash" to ~/.bashrc.
_aivoicebus() {
  local IFS=$'\n'
  COMPREPLY=($(aivoicebus __complete "${COMP_WORDS[@]:1:COMP_CWORD}" 2>/dev/null))
}
complete -o default -F _aivoicebus aivoicebus
`

const fishCompletion = `# aivoicebus completion for fish: aivoicebus completion fish > ~/.config/fish/completions/aivoicebus.fish
complete -c aivoicebus -f -a '(aivoicebus __complete (commandline -opc)[2..-1] (commandline -ct) 2>/dev/null)'
`
