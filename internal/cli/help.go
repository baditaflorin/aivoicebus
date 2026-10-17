package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// helpTopics is the one-line summary of every command, in the order `help` lists them. A test
// holds it to the command table, so a command cannot be added without a line here.
var helpTopics = []struct{ name, summary string }{
	{"send", "send a message to a session, a machine or everyone"},
	{"reply", "answer the last message you read, to the right session"},
	{"inbox", "read your messages (--ack marks them read)"},
	{"ack", "mark messages read"},
	{"retract", "withdraw a message you sent (undo send)"},
	{"thread", "show a conversation"},
	{"who", "list the sessions on the mesh"},
	{"whoami", "print this session's identity"},
	{"name", "give this session a persistent name"},
	{"role", "claim or release a role other sessions can address"},
	{"quiet", "set quiet hours for non-urgent messages"},
	{"serve", "run a node (loopback by default; see --addr)"},
	{"join", "connect this machine to an existing mesh"},
	{"setup", "install the hooks and instructions for Claude Code, Codex and Gemini"},
	{"uninstall", "remove what setup added (--dry-run shows it, --purge removes all)"},
	{"doctor", "check this machine's setup (--fix repairs it)"},
	{"mesh", "check that the nodes agree"},
	{"nodes", "show or refresh the node list"},
	{"selftest", "send a probe through the mesh and read it back"},
	{"hook", "the session hook the tools run (not for people)"},
	{"mcp", "run the MCP server for tools that speak MCP"},
	{"export", "write the message log to a file"},
	{"import", "load an exported log"},
	{"update", "install a newer signed release (--from github|mesh|dir:PATH)"},
	{"trust-key", "list, add or remove the public keys this machine trusts for updates"},
	{"release", "for maintainers: keygen, pack, verify, nodes"},
	{"secret", "hand a credential to the agents on this machine without pasting it"},
	{"get", "download a file a message points at"},
	{"tail", "follow or push a live log a message points at"},
	{"view", "a read-only page of the mesh: session cards and the conversations between them (--demo to try it)"},
	{"task", "give a machine a task: every session there sees it until one claims it"},
	{"claim", "take a task for this session (the first claim wins)"},
	{"queue", "list the tasks waiting for this session's machine"},
	{"wait", "block until a message arrives (in a thread, or from someone), for agent-to-agent conversations"},
	{"handoff", "hand work to another agent: a task with the goal, repo, branch, commit, push state and (--diff) your changes"},
	{"keys", "this machine's signing key, and the keys pinned for other machines (forget one after a reinstall)"},
	{"room", "a standing conversation of several agents: join, say, wait, who, invite"},
	{"lease", "take, release or list a lease on a file, branch or anything named, so two agents do not edit it at once"},
	{"status", "did it get through: who read a message and when, who took a task, how many replies"},
	{"ping", "who is there: the sessions a target reaches and how recently each was seen"},
	{"worker", "run a command (an agent) for tasks from allowed senders while no session is open; opt-in, see `worker allow`"},
	{"usage", "print the agent-facing usage text"},
	{"version", "print the version"},
	{"help", "this list; `help <command>` shows that command's flags"},
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, "aivoicebus: a message bus for AI coding agents on different machines")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "usage: aivoicebus <command> [flags]")
	fmt.Fprintln(w)
	width := 0
	for _, t := range helpTopics {
		width = max(width, len(t.name))
	}
	for _, t := range helpTopics {
		fmt.Fprintf(w, "  %-*s  %s\n", width, t.name, t.summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "`aivoicebus <command> -h` shows a command's flags. `aivoicebus usage` is the text agents read.")
}

// helpCmd is `help` and `help <command>`. It never runs the command it is asked about: some
// read stdin or start a server, so a command's flags are read with `aivoicebus <command> -h`.
func helpCmd(args []string, s Streams) error {
	if len(args) == 0 || args[0] == "help" {
		printHelp(s.Out)
		return nil
	}
	for _, t := range helpTopics {
		if t.name == args[0] {
			fmt.Fprintf(s.Out, "%s: %s\nits flags: aivoicebus %s -h\n", t.name, t.summary, t.name)
			return nil
		}
	}
	names := make([]string, 0, len(helpTopics))
	for _, t := range helpTopics {
		names = append(names, t.name)
	}
	sort.Strings(names)
	return fmt.Errorf("unknown command %q (commands: %s)", args[0], strings.Join(names, ", "))
}
