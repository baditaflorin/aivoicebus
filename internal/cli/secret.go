package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/credstore"
)

const secretUsage = `usage: aivoicebus secret save NAME [--host HOST[,HOST]] [--check-url URL [--check-field F]] [--no-check]
       aivoicebus secret list
       aivoicebus secret path NAME
       aivoicebus secret rm NAME
       aivoicebus secret curl NAME [--header 'Name: {}'] -- https://HOST/... [CURL-ARGS...]

Hand a credential (a CI or GitHub token, say) to the agents on this machine without
pasting it into a chat: you type it here, once. It is kept in ~/.config/aivoicebus/credentials/NAME/
(mode 600) together with the hosts it may be sent to. An agent uses it with 'aivoicebus secret curl',
which puts it in a header, never in an argument, and masks it if it is ever echoed.`

// secretCmd keeps credentials that a person hands to the agents on their machine.
func secretCmd(args []string, s Streams) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(s.Err, secretUsage)
		if len(args) > 0 {
			return exitError{0}
		}
		return exitError{2}
	}
	base, err := secretBase()
	if err != nil {
		return err
	}
	switch args[0] {
	case "save":
		return secretSave(base, args[1:], s)
	case "list":
		return secretList(base, s)
	case "path":
		return secretPath(base, args[1:], s)
	case "rm", "remove", "delete":
		return secretRm(base, args[1:], s)
	case "curl":
		return secretCurl(base, args[1:], s)
	}
	fmt.Fprintln(s.Err, secretUsage)
	return exitError{2}
}

// secretBase is ~/.config/aivoicebus/credentials, a directory of this tool's own: no other
// program's files are ever in it, so a name can never collide with one.
func secretBase() (string, error) {
	d := config.Dir()
	if d == "" {
		return "", errors.New("no home directory to keep credentials in")
	}
	return filepath.Join(d, "credentials"), nil
}

// allowLoopbackCheck lets a check go to a local address: only for tests, because a program on
// this machine can listen there, and it would be handed the token.
func allowLoopbackCheck() bool { return os.Getenv("AIVOICEBUS_ALLOW_LOCAL_CHECK") == "1" }

func secretSave(base string, args []string, s Streams) error {
	fs := newFlagSet("secret save")
	checkURL := fs.String("check-url", "", "ask this https URL who the token belongs to (a bearer request); default: the service's own for github")
	checkField := fs.String("check-field", "login", "the JSON field of the answer that names the owner")
	noCheck := fs.Bool("no-check", false, "save without asking the service")
	hostFlag := fs.String("host", "", "the host(s) the token may be sent to by 'secret curl' (comma-separated); the check URL's host is added")
	// the name may come before or after the flags
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if rest := fs.Args(); name == "" && len(rest) == 1 {
		name = rest[0]
	} else if len(rest) > 0 || name == "" {
		return errors.New("usage: aivoicebus secret save NAME [--host HOST] [--check-url URL] [--no-check]")
	}
	if err := credstore.ValidName(name); err != nil {
		return err
	}

	// Where the token will be sent is said BEFORE it is asked for, and again in the prompt
	// line on the terminal (stderr can be thrown away): a custom address someone else
	// suggested is the way to get a token sent to the wrong place.
	url, field := *checkURL, *checkField
	if url == "" {
		if p, ok := credstore.Presets[name]; ok {
			url, field = p.URL, p.Field
		}
	}
	var hosts []string
	for _, h := range strings.Split(*hostFlag, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	prompt := "Credential for " + name + " (paste it; nothing is shown): "
	if url != "" {
		host, err := credstore.HostOf(url, allowLoopbackCheck())
		if err != nil {
			return fmt.Errorf("check address: %v (plain http to localhost is only for tests)", err)
		}
		hosts = append(hosts, host)
		if !*noCheck {
			how := "built-in check"
			if *checkURL != "" {
				how = "an address given on the command line: make sure you trust it"
			}
			fmt.Fprintf(s.Err, "The token will be sent once, to %s (%s), to check whose it is. It is not sent anywhere else.\n", host, how)
			prompt = fmt.Sprintf("Credential for %s (checked once at %s; paste it, nothing is shown): ", name, host)
		}
	}

	raw, err := readSecret(s, prompt, "pbpaste | aivoicebus secret save NAME")
	if err != nil {
		return err
	}
	tok, err := credstore.Clean(raw)
	if err != nil {
		return err
	}

	owner := ""
	if url != "" && !*noCheck {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if owner, err = credstore.Check(ctx, &http.Client{}, url, field, tok, allowLoopbackCheck()); err != nil {
			return fmt.Errorf("%v; nothing was saved (--no-check saves it anyway)", err)
		}
	}
	if err := credstore.Save(base, name, tok, hosts); err != nil {
		return err
	}
	p := credstore.Path(base, name)
	bound, _ := credstore.Hosts(base, name)
	where := "It may be sent to: " + strings.Join(bound, ", ") + "."
	if len(bound) == 0 {
		where = "It is not bound to any host yet, so 'secret curl' will refuse it: save it again with --host HOST."
	}
	switch {
	case owner != "":
		fmt.Fprintf(s.Out, "saved %s: the token is valid for %q. It is in %s (mode 600). %s\n", name, owner, p, where)
	case *noCheck:
		fmt.Fprintf(s.Out, "saved %s, unchecked, in %s (mode 600). %s\n", name, p, where)
	default:
		fmt.Fprintf(s.Out, "saved %s in %s (mode 600). It was not checked: there is no check for %q (pass --check-url to add one). %s\n", name, p, name, where)
	}
	return nil
}

// readSecret takes the credential from a pipe or, at a terminal, from a prompt that does not
// echo. pipeExample is the command line shown when there is no terminal to ask on.
func readSecret(s Streams, prompt, pipeExample string) (string, error) {
	if in := stdinOrNil(s.In); in != nil {
		b, err := io.ReadAll(io.LimitReader(in, credstore.MaxLen+2))
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return readHidden(prompt, pipeExample)
}

// readHidden asks on the terminal with echo off, and puts the terminal back however it ends:
// on a return, and on Ctrl-C, a hangup, a termination or a quit.
func readHidden(prompt, pipeExample string) (string, error) {
	pipeHint := "pipe the credential in instead, for example:  " + pipeExample
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("no terminal to ask on here: " + pipeHint)
	}
	defer tty.Close()
	stty := func(arg string) error {
		c := exec.Command("stty", arg)
		c.Stdin = tty
		return c.Run()
	}
	// signals are caught BEFORE echo goes off, so there is no window with a silent terminal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	defer signal.Stop(sig)
	if _, err := exec.LookPath("stty"); err != nil {
		return "", errors.New("cannot hide the typing here (no stty): " + pipeHint)
	}
	if err := stty("-echo"); err != nil {
		return "", errors.New("cannot hide the typing here: " + pipeHint)
	}
	go func() {
		select {
		case <-sig: // do not leave the terminal silent
			_ = stty("echo")
			fmt.Fprintln(tty)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		close(done)
		_ = stty("echo")
		fmt.Fprintln(tty)
	}()
	fmt.Fprint(tty, prompt)
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if len(strings.TrimSpace(line)) >= 1000 {
		return "", errors.New("a terminal cuts a pasted line at about 1000 characters, and this one is that long, so it may be truncated: " + pipeHint)
	}
	return line, nil
}

func secretList(base string, s Streams) error {
	es, err := credstore.List(base)
	if err != nil {
		return err
	}
	if len(es) == 0 {
		fmt.Fprintln(s.Out, "no credentials saved here (aivoicebus secret save NAME)")
		return nil
	}
	tw := tabwriter.NewWriter(s.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSAVED\tMODE\tMAY BE SENT TO")
	for _, e := range es {
		hosts := strings.Join(e.Hosts, ",")
		if hosts == "" {
			hosts = "(no host: not usable by 'secret curl')"
		}
		if !e.Private {
			hosts += "  <- readable by others: chmod 600 " + e.Path
		}
		fmt.Fprintf(tw, "%s\t%s\t%04o\t%s\n", e.Name, e.Saved.Local().Format("2006-01-02 15:04"), e.Mode, hosts)
	}
	return tw.Flush()
}

func secretPath(base string, args []string, s Streams) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: aivoicebus secret path NAME")
	}
	if _, err := credstore.Load(base, args[0]); err != nil { // exists, private, well-formed
		return err
	}
	fmt.Fprintln(s.Out, credstore.Path(base, args[0]))
	return nil
}

func secretRm(base string, args []string, s Streams) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: aivoicebus secret rm NAME")
	}
	if err := credstore.Remove(base, args[0]); err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "removed %s. If it was a real token, also revoke it at its source.\n", args[0])
	return nil
}

// secretCurl runs curl with the credential in a header given on stdin, so the value is in no
// argument (ps shows arguments). curl is run without reading ~/.curlrc, with the options that
// print or redirect the header refused, the destination limited to the hosts the credential
// was saved for, and everything curl prints passed through a filter that masks the value.
func secretCurl(base string, args []string, s Streams) error {
	fs := newFlagSet("secret curl")
	header := fs.String("header", "Authorization: Bearer {}", "the header to send, with {} where the credential goes")
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	rest := fs.Args()
	if name == "" && len(rest) > 0 { // flags first: curl --header X NAME -- URL
		name, rest = rest[0], rest[1:]
	}
	if name == "" {
		return errors.New("usage: aivoicebus secret curl NAME [--header 'Name: {}'] -- https://HOST/... [CURL-ARGS...]")
	}
	tok, err := credstore.Load(base, name)
	if err != nil {
		return err
	}
	hosts, err := credstore.Hosts(base, name)
	if err != nil {
		return err
	}
	if _, err := credstore.CheckCurlArgs(rest, hosts); err != nil {
		return err
	}
	cfg, err := credstore.CurlConfig(*header, tok)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("curl")
	if err != nil {
		return errors.New("curl is not installed")
	}
	argv := append([]string{"-q", "-K", "-", "--proto", "=https"}, rest...) // -q first: no ~/.curlrc
	c := exec.Command(bin, argv...)
	c.Stdin = strings.NewReader(cfg)
	out, errw := credstore.Redactor(s.Out, tok), credstore.Redactor(s.Err, tok)
	c.Stdout, c.Stderr = out, errw
	runErr := c.Run()
	_ = out.Close()
	_ = errw.Close()
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return exitError{ee.ExitCode()}
		}
		return runErr
	}
	return nil
}
