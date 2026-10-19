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
	{"start", "the guided first run: reach the mesh, set up the tools, Codex trust, approver key, phone alerts, doctor, and a hello"},
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
	{"token", "hand a machine its own machine token (`token hand HOST`, where it was made) and install it there (`token install ID`, a person approves); the nodes confirm the switch; `token report`: which machines still use the shared token"},
	{"get", "download a file a message points at"},
	{"tail", "follow or push a live log a message points at"},
	{"view", "a read-only page of the mesh: session cards and the conversations between them (--demo to try it)"},
	{"demo", "see the bus work, offline: two throwaway nodes, two invented agents, a task, a file and the brakes, narrated (--keep to poke around)"},
	{"task", "give a machine a task: every session there sees it until one claims it (--person: for the person there, never an agent)"},
	{"ask", "ask another machine's agent and get the answer in one command: picks the session listening now (or seen last), refuses to guess between look-alikes"},
	{"claim", "take a task for this session (the first claim wins)"},
	{"queue", "list the tasks waiting for this session's machine"},
	{"wait", "block until a message arrives (in a thread, or from someone), for agent-to-agent conversations"},
	{"handoff", "hand work to another agent: a task with the goal, repo, branch, commit, push state and (--diff) your changes"},
	{"keys", "this machine's signing key, and the keys pinned for other machines (forget one after a reinstall)"},
	{"room", "a standing conversation of several agents: join, say, wait, who, invite"},
	{"lease", "take, release or list a lease on a file, branch or anything named, so two agents do not edit it at once"},
	{"status", "where this session stands, in one screen, each problem with its fix; `status ID`: did that message get through"},
	{"ping", "who is there: the sessions a target reaches and how recently each was seen"},
	{"worker", "answer tasks from allowed senders while no session is open: `worker install --agent codex` keeps one running; opt-in, see `worker allow`"},
	{"tokens", "what aivoicebus cost on this machine: context it put into each session (estimated) and the worker's runs (reported tokens)"},
	{"audit", "what was approved, run, stopped, released or allowed on this machine; --verify checks the chain and signatures"},
	{"ask-human", "ask this machine's person a question and wait: they answer on the phone (choice buttons) or with `aivoicebus answer`"},
	{"answer", "the person's side of ask-human and stuck: list the open questions and blocked agents, or answer one (in a terminal)"},
	{"progress", "say how a task goes, to whoever waits on it: progress --re ID \"step 3/5\" (waits show it and go on)"},
	{"stuck", "say you are blocked instead of going silent: stuck \"blocked on X\" [--re ID] [--need person|peer|TOOL]; the task's sender and your person (or a listening peer) are told"},
	{"held", "what was held back from agents (a bad signature, or text that reads like instructions to an agent), with the text: the user's, in a terminal"},
	{"unhold", "let agents see a held message (the user, at a terminal)"},
	{"incident", "a credential leaked: plan (default) or do, at a terminal, the whole response: revoke or rotate it, retract the messages that held it, who read them, an audit entry"},
	{"pause", "the kill switch: workers stop and take no task, schedules hold, sessions are told to stop (all, or a machine; --for 1h)"},
	{"resume", "lift the pauses this machine sent (a person at a terminal)"},
	{"cancel", "take back a task you sent: a worker running it stops the run (and everything it started) within seconds"},
	{"approve", "approve a write-mode worker's task (the id of its [approval needed] message), with the approver passphrase on a terminal; `deny ID` refuses it"},
	{"approver", "init: make the approver key (sealed with a passphrase only you know); show: its public half, for worker --approver-key"},
	{"deny", "refuse a write-mode worker's task: it answers that it did not run"},
	{"schedule", "recurring tasks with no person and no cron: add NAME (--every 6h | --at 09:00) MACHINE \"...\"; the worker sends them when due"},
	{"watch", "alert (a line, and a POST to `watch notify set URL`) when a task this machine sent waits untaken; the worker runs it with --watch"},
	{"trust-codex", "trust the Codex hooks setup installed (Codex runs new hooks only once you trust them); --check only reports"},
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
	return fmt.Errorf("unknown command %q (commands: %s): `aivoicebus help COMMAND` says what one does", args[0], strings.Join(names, ", "))
}
