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
	{"amend", "replace a message you sent with a corrected text: sessions that did not read it get only the new one, those that did get it marked [amended]"},
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
	{"fleet", "every machine heard from lately: its release, last heard and token, as the nodes saw them; `fleet update` asks the person on each machine that is behind to run aivoicebus update"},
	{"token", "hand a machine its own machine token (`token hand HOST`, where it was made) and install it there (`token install ID`, a person approves); the nodes confirm the switch; `token report`: which machines still use the shared token"},
	{"get", "download a file a message points at"},
	{"share", "a thread's shared space, kept for days and encrypted for its machines: put ID FILE (--diff: git diff), get ID NAME [--version N], ls ID, notes ID [--edit --base N], add ID HOST; put --via pipe hands a file over once"},
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
	{"lease", "take, release or list a lease on a file, branch or anything named, so two agents do not edit it at once; `lease run KEY -- CMD` holds one while CMD runs and frees it after"},
	{"status", "where this session stands, in one screen, each problem with its fix; `status ID`: did that message get through"},
	{"ping", "who is there: the sessions a target reaches and how recently each was seen"},
	{"worker", "answer tasks from allowed senders while no session is open: `worker install --agent codex` keeps one running; opt-in, see `worker allow`"},
	{"tokens", "what aivoicebus cost on this machine: context it put into each session (estimated) and the worker's runs (reported tokens)"},
	{"audit", "what was approved, run, stopped, released or allowed on this machine; --verify checks the chain and signatures"},
	{"ask-human", "ask this machine's person a question and wait: they answer on the phone (choice buttons) or with `aivoicebus answer`"},
	{"answer", "the person's side of ask-human and stuck: list the open questions and blocked agents, or answer one (in a terminal)"},
	{"state", "a conversation's handshake, seen by both sides: state ID ready \"note\" moves it (offered, taken, ready, doing, done, stuck, declined), --wait-state ready 10m waits for it"},
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
	{"completion", "print tab completion for zsh, bash or fish: commands, subcommands and flags (`aivoicebus completion zsh > \"${fpath[1]}/_aivoicebus\"`)"},
	{"__complete", "the candidates for the last word, for the completion scripts (not for people)"},
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
			fmt.Fprintf(s.Out, "%s: %s\n", t.name, t.summary)
			if ex := helpExamples[t.name]; len(ex) > 0 {
				fmt.Fprintln(s.Out, "\nexamples:")
				for _, e := range ex {
					fmt.Fprintf(s.Out, "  %s\n", e)
				}
				fmt.Fprintln(s.Out)
			}
			fmt.Fprintf(s.Out, "its flags: aivoicebus %s -h\n", t.name)
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

// helpExamples are real command lines for the commands people reach for first (`help COMMAND`).
// A test runs each one's flags through the command's own parser, so an example cannot rot.
var helpExamples = map[string][]string{
	"share": {"aivoicebus share put 4f2a9c1e0b build.log         # a new version of build.log in that thread's space",
		"aivoicebus share put 4f2a9c1e0b --diff              # your uncommitted changes, as changes.diff",
		"aivoicebus share get 4f2a9c1e0b build.log --version 1 --out old.log",
		"aivoicebus share notes 4f2a9c1e0b --edit --base 3 --file notes.md   # refused if someone wrote version 4 first"},
	"start":      {"aivoicebus start                 # the guided first run on a new machine"},
	"send":       {`aivoicebus send --to laptop-b "the build is green"          # every session on a machine`, `aivoicebus send --to laptop-b/codex.k3 "look at PR 12" --prio urgent`},
	"reply":      {`aivoicebus reply --re 4f2a "done, see 9c1e" --end`, `aivoicebus reply --re 4f2a --wait-reply 10m "which branch?"   # answer and wait for the next turn`},
	"inbox":      {"aivoicebus inbox            # unread, without marking them read", "aivoicebus inbox --ack      # read and mark read"},
	"task":       {`aivoicebus task build-1 "run the suite" --wait-reply 15m`, `aivoicebus task laptop-b --person "plug in the phone"   # for the person, never an agent`},
	"ask":        {`aivoicebus ask laptop-b "does the login fix pass on your branch?"`, `aivoicebus ask --any --tool codex "review this diff" --json`},
	"status":     {"aivoicebus status            # one screen: who you are, token, nodes, what waits", "aivoicebus status --json"},
	"fleet":      {"aivoicebus fleet             # every machine: release, last heard, token", "aivoicebus fleet update      # the plan; --apply asks the person on each to update"},
	"token":      {"aivoicebus token report      # which machines still use the shared token", "aivoicebus token hand laptop-b   # on the deploy machine; laptop-b's person runs token install"},
	"stuck":      {`aivoicebus stuck "waiting for the staging DB" --re 4f2a`},
	"pause":      {`aivoicebus pause "stop, I am looking"        # every agent on the mesh`, `aivoicebus pause laptop-b --for 30m "deploying"`},
	"completion": {`aivoicebus completion zsh > "${fpath[1]}/_aivoicebus"   # then open a new shell`, "aivoicebus completion fish > ~/.config/fish/completions/aivoicebus.fish"},
}
