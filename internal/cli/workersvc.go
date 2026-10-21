package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// The worker as a service of the user's own (a launchd agent on macOS, a systemd user unit on
// Linux): `worker install` writes it and starts it, so tasks sent to this machine are answered while
// no session is open, after a restart too. It is the files of deploy/worker/, filled in.

const (
	svcLabel = "org.aivoicebus.worker"
	svcUnit  = "aivoicebus-worker.service"
)

// svcSpec is what the service runs and with what.
type svcSpec struct {
	Argv []string // the aivoicebus binary, "worker", and its flags
	Dir  string   // the working directory: empty, so a task never runs inside a repository
	Path string   // PATH for the worker and the agent it starts
	URLs string   // AIVOICEBUS_URL: a service does not read the shell's env file
	Log  string   // where its own output goes (launchd); the decisions are in worker.log
}

func svcPaths(home string) (file, log string) {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "LaunchAgents", svcLabel+".plist"), filepath.Join(home, "Library", "Logs", "aivoicebus-worker.log")
	}
	return filepath.Join(home, ".config", "systemd", "user", svcUnit), ""
}

// launchdPlist is the launchd agent for spec: started at login, restarted when it fails.
func launchdPlist(sp svcSpec) string {
	x := html.EscapeString
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- written by aivoicebus worker install; aivoicebus worker uninstall removes it -->
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + svcLabel + `</string>
  <key>ProgramArguments</key>
  <array>
`)
	for _, a := range sp.Argv {
		b.WriteString("    <string>" + x(a) + "</string>\n")
	}
	b.WriteString(`  </array>
  <key>WorkingDirectory</key>
  <string>` + x(sp.Dir) + `</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>` + x(sp.Path) + `</string>
    <key>AIVOICEBUS_URL</key>
    <string>` + x(sp.URLs) + `</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ThrottleInterval</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>` + x(sp.Log) + `</string>
  <key>StandardErrorPath</key>
  <string>` + x(sp.Log) + `</string>
</dict>
</plist>
`)
	return b.String()
}

// systemdQuote quotes one word of an ExecStart line.
func systemdQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"'\\$%;") {
		return s
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `$$`, `%`, `%%`)
	return `"` + r.Replace(s) + `"`
}

// systemdUnit is the systemd user unit for spec: restarted when it fails, started at login (and
// while logged out once lingering is on).
func systemdUnit(sp svcSpec) string {
	words := make([]string, len(sp.Argv))
	for i, a := range sp.Argv {
		words[i] = systemdQuote(a)
	}
	return `# written by aivoicebus worker install; aivoicebus worker uninstall removes it
[Unit]
Description=aivoicebus worker (runs an agent on tasks from allowed senders)
After=network-online.target

[Service]
WorkingDirectory=` + sp.Dir + `
Environment=` + systemdQuote("PATH="+sp.Path) + `
Environment=` + systemdQuote("AIVOICEBUS_URL="+sp.URLs) + `
ExecStart=` + strings.Join(words, " ") + `
Restart=on-failure
RestartSec=30
NoNewPrivileges=yes
PrivateTmp=yes

[Install]
WantedBy=default.target
`
}

func workerSvcCmd(sub string, args []string, s Streams) error {
	switch sub {
	case "install":
		return workerInstall(args, s)
	case "uninstall":
		return workerUninstall(s)
	default:
		return workerStatus(s)
	}
}

func workerInstall(args []string, s Streams) error {
	fs := newFlagSet("worker install")
	agent := fs.String("agent", "", "the agent it runs, read-only: "+strings.Join(worker.PresetNames(), " | "))
	execT := fs.String("exec", "", "instead of --agent, the command to run for each task (see `worker --exec`)")
	timeout := fs.Duration("timeout", 10*time.Minute, "kill a run after this long")
	maxAge := fs.Duration("max-age", time.Hour, "leave alone a task older than this")
	allowUnsigned := fs.Bool("allow-unsigned", false, "also take unsigned tasks (senders older than 0.7.0)")
	write := fs.Bool("write", false, "let the agent change files, in worktrees of --repo, after --approver approves each task (see `worker --write`)")
	repo := fs.String("repo", "", "with --write: the git repository")
	approver := fs.String("approver", "", "with --write: who approves each task")
	approveWithin := fs.Duration("approve-within", 30*time.Minute, "with --write: how long to wait for the approval")
	approverKey := fs.String("approver-key", "", "with --write: the approver key's public half (`aivoicebus approver show`)")
	budget := fs.Int("budget", 0, "tokens the runs may use in any 24 hours (see `worker --budget`)")
	sandbox := fs.Bool("sandbox", true, "run each command without access to this user's secrets (see `worker --sandbox`)")
	watchFor := fs.Duration("watch", 0, "also alert when a task this machine sent waits untaken this long (see `aivoicebus watch`)")
	testLane := fs.Bool("test-lane", false, "opt the worker in to the test lane (see `worker --test-lane`): `aivoicebus selftest --worker` can then check it from another machine")
	dry := fs.Bool("dry-run", false, "print the service file and change nothing")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*agent == "") == (*execT == "") {
		return outbox.Usagef("usage: worker install --agent %s | --exec 'COMMAND'  [--timeout 10m] [--max-age 1h] [--dry-run]", strings.Join(worker.PresetNames(), "|"))
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return errors.New("worker install knows launchd (macOS) and systemd (Linux) only: run `aivoicebus worker` in a terminal instead")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin := filepath.Join(home, ".local", "bin", "aivoicebus")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("no %s: run `aivoicebus setup` first (the service runs the installed binary, which `update` keeps current)", bin)
	}
	// The agent's own command, found now: a service starts with almost no PATH.
	var cmd0 string
	argv := []string{bin, "worker"}
	if *agent != "" {
		a := strings.ToLower(*agent)
		p, ok := worker.PresetArgv(a)
		if !ok {
			return outbox.Usagef("--agent is one of %s", strings.Join(worker.PresetNames(), ", "))
		}
		cmd0 = p[0]
		argv = append(argv, "--agent", a)
	} else {
		w, err := worker.SplitCommand(*execT)
		if err != nil {
			return outbox.Usagef("%v", err)
		}
		cmd0 = w[0]
		argv = append(argv, "--exec", *execT)
	}
	found, err := exec.LookPath(cmd0)
	if err != nil && cmd0 == "codex" {
		if _, e := os.Stat(codexBundled); e == nil {
			found, err = codexBundled, nil
		}
	}
	if err != nil {
		return fmt.Errorf("%s is not on PATH: ask the user to install it, or run it again with its full path in --exec", cmd0)
	}
	if *timeout != 10*time.Minute {
		argv = append(argv, "--timeout", timeout.String())
	}
	if *maxAge != time.Hour {
		argv = append(argv, "--max-age", maxAge.String())
	}
	if *allowUnsigned {
		argv = append(argv, "--allow-unsigned")
	}
	if *testLane {
		argv = append(argv, "--test-lane")
	}
	if *watchFor > 0 {
		argv = append(argv, "--watch", watchFor.String())
	}
	if !*sandbox {
		argv = append(argv, "--sandbox=false")
	}
	if *budget > 0 {
		argv = append(argv, "--budget", strconv.Itoa(*budget))
	}
	if *write {
		if *repo == "" || *approver == "" || *approverKey == "" {
			return outbox.Usagef("--write needs --repo PATH, --approver WHO and --approver-key KEY (`aivoicebus approver show` where the approver is)")
		}
		abs, err := filepath.Abs(*repo)
		if err != nil {
			return err
		}
		argv = append(argv, "--write", "--repo", abs, "--approver", *approver, "--approver-key", *approverKey)
		if *approveWithin != 30*time.Minute {
			argv = append(argv, "--approve-within", approveWithin.String())
		}
	}
	urls := strings.Join(config.URLs(), ",")
	if urls == "" {
		return errors.New("no node configured: run `aivoicebus setup --url ...` first")
	}
	allow, err := worker.LoadAllow()
	if err != nil {
		return err
	}
	if allow.Empty() && !*dry {
		return errors.New("no sender is allowed, so the worker would run nothing: allow one first with `aivoicebus worker allow add MACHINE`")
	}
	file, log := svcPaths(home)
	paths := []string{filepath.Dir(found), filepath.Join(home, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"}
	sp := svcSpec{Argv: argv, Dir: filepath.Join(home, "aivoicebus-worker"), Path: strings.Join(dedupe(paths), ":"), URLs: urls, Log: log}
	text := systemdUnit(sp)
	if runtime.GOOS == "darwin" {
		text = launchdPlist(sp)
	}
	if *dry {
		fmt.Fprintf(s.Out, "would write %s:\n%s", file, text)
		return nil
	}
	if err := os.MkdirAll(sp.Dir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "wrote %s: runs %s for tasks from %s\n", file, worker.QuoteCommand(argv[1:]), allowedText(allow))
	config.Audit(identity.Me(), "worker install", worker.QuoteCommand(argv[1:]))
	if err := svcStart(file); err != nil {
		return fmt.Errorf("written, but not started: %v", err)
	}
	time.Sleep(2 * time.Second)
	up, detail := svcRunning()
	if !up {
		return fmt.Errorf("started, but it is not running (%s): see `aivoicebus worker status`", detail)
	}
	fmt.Fprintf(s.Out, "running (%s). Tasks sent to this machine are answered while no session is open; it shows in `who` as listening.\n", detail)
	if runtime.GOOS == "linux" {
		fmt.Fprintln(s.Out, "To keep it running while you are logged out: loginctl enable-linger \"$USER\"")
	}
	fmt.Fprintln(s.Out, "Check: aivoicebus worker status    Stop and remove: aivoicebus worker uninstall")
	return nil
}

func dedupe(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func svcStart(file string) error {
	if runtime.GOOS == "darwin" {
		dom := "gui/" + strconv.Itoa(os.Getuid())
		_ = exec.Command("launchctl", "bootout", dom+"/"+svcLabel).Run() // an older copy; none is fine
		time.Sleep(time.Second)
		if out, err := exec.Command("launchctl", "bootstrap", dom, file).CombinedOutput(); err != nil {
			return fmt.Errorf("launchctl bootstrap: %v %s: run `aivoicebus doctor`, and ask the user", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", svcUnit}, {"restart", svcUnit}} {
		if out, err := exec.Command("systemctl", append([]string{"--user"}, a...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %v %s: run `aivoicebus doctor`, and ask the user", strings.Join(a, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// svcRunning reports whether the service runs now, with launchd's or systemd's word for it.
func svcRunning() (bool, string) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+svcLabel).CombinedOutput()
		if err != nil {
			return false, "not loaded"
		}
		state, pid := "", ""
		for _, line := range strings.Split(string(out), "\n") {
			t := strings.TrimSpace(line)
			if v, ok := strings.CutPrefix(t, "state = "); ok && state == "" {
				state = v
			}
			if v, ok := strings.CutPrefix(t, "pid = "); ok && pid == "" {
				pid = v
			}
		}
		if state == "running" {
			return true, "pid " + pid
		}
		return false, "state " + state
	}
	out, _ := exec.Command("systemctl", "--user", "is-active", svcUnit).Output()
	st := strings.TrimSpace(string(out))
	return st == "active", st
}

func workerUninstall(s Streams) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	file, _ := svcPaths(home)
	if runtime.GOOS == "darwin" {
		_ = exec.Command("launchctl", "bootout", "gui/"+strconv.Itoa(os.Getuid())+"/"+svcLabel).Run()
	} else {
		_ = exec.Command("systemctl", "--user", "disable", "--now", svcUnit).Run()
	}
	switch err := os.Remove(file); {
	case err == nil:
		fmt.Fprintf(s.Out, "stopped the worker and removed %s (the allowlist and worker.log stay)\n", file)
	case os.IsNotExist(err):
		fmt.Fprintln(s.Out, "no worker service installed; nothing to remove")
	default:
		return err
	}
	if runtime.GOOS == "linux" {
		_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	}
	return nil
}

func workerStatus(s Streams) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	file, _ := svcPaths(home)
	if _, err := os.Stat(file); err != nil {
		fmt.Fprintln(s.Out, "service:  not installed (aivoicebus worker install --agent claude|codex|gemini|grok)")
	} else {
		up, detail := svcRunning()
		state := "installed, NOT running"
		if up {
			state = "running"
		}
		fmt.Fprintf(s.Out, "service:  %s (%s), %s\n", state, detail, file)
	}
	if allow, err := worker.LoadAllow(); err != nil {
		fmt.Fprintf(s.Out, "allowed:  %v\n", err)
	} else {
		fmt.Fprintf(s.Out, "allowed:  %s\n", allowedText(allow))
	}
	// The last decisions, from the worker's own log (no task text in it).
	f, err := os.Open(worker.LogPath())
	if err != nil {
		fmt.Fprintln(s.Out, "recent:   nothing logged yet")
		return nil
	}
	defer f.Close()
	var last []worker.Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e worker.Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			last = append(last, e)
			if len(last) > 5 {
				last = last[1:]
			}
		}
	}
	fmt.Fprintln(s.Out, "recent:")
	for _, e := range last {
		detail := e.Why
		if e.Exit != nil {
			detail = fmt.Sprintf("exit %d after %s", *e.Exit, e.Took)
		}
		fmt.Fprintf(s.Out, "  %s  %-8s task %s from %s  %s\n", view.WhenSec(e.At, time.Now()), e.Outcome, e.Task, e.From, detail)
	}
	return nil
}
