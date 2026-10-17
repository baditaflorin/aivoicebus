package worker

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits bound one run.
type Limits struct {
	Timeout   time.Duration // the run is killed (its whole process group) after this long
	MaxOutput int           // bytes of stdout, and of stderr, kept; the rest is read and dropped
}

// Result is how one run went.
type Result struct {
	Stdout, Stderr string
	Truncated      bool // stdout or stderr went past MaxOutput
	ExitCode       int  // -1 when the command did not exit by itself (killed, or never started)
	TimedOut       bool // killed at the time limit
	Stopped        bool // killed because the worker was stopped
	StartErr       error
	Took           time.Duration
}

// Exec runs argv once with stdin as its standard input and env as its environment. The text is
// never part of argv or of a shell command line. When the run ends, anything it left running in its
// process group is killed too.
func Exec(ctx context.Context, argv []string, stdin string, env []string, l Limits) Result {
	start := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, l.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	out, errOut := &capped{max: l.MaxOutput}, &capped{max: l.MaxOutput}
	cmd.Stdout, cmd.Stderr = out, errOut
	cmd.WaitDelay = 5 * time.Second // a grandchild holding the pipes open cannot keep the worker waiting
	ownGroup(cmd)
	r := Result{ExitCode: -1}
	if err := cmd.Start(); err != nil {
		r.StartErr = err
		r.Took = time.Since(start)
		return r
	}
	err := cmd.Wait()
	killGroup(cmd)
	r.Took = time.Since(start)
	r.Stdout, r.Stderr = out.String(), errOut.String()
	r.Truncated = out.over || errOut.over
	var ee *exec.ExitError
	switch {
	case ctx.Err() != nil:
		r.Stopped = true
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		r.TimedOut = true
	case err == nil:
		r.ExitCode = 0
	case errors.As(err, &ee) && ee.Exited():
		r.ExitCode = ee.ExitCode()
	}
	return r
}

// capped keeps the first max bytes written to it and counts the rest as dropped, so a command that
// prints without end never blocks on a full pipe and never fills the worker's memory.
type capped struct {
	b    []byte
	max  int
	over bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - len(c.b); room > 0 {
		c.b = append(c.b, p[:min(room, len(p))]...)
		if len(p) > room {
			c.over = true
		}
	} else if len(p) > 0 {
		c.over = true
	}
	return len(p), nil
}

// String is the kept bytes, cut back to whole UTF-8 characters.
func (c *capped) String() string {
	b := c.b
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

// SplitCommand turns the --exec template into argv the way a shell splits words: spaces separate,
// '...' keeps everything, "..." keeps everything but \" and \\, and \ escapes one character. It
// expands nothing ($VAR, ~, globs, $(...)), and no shell ever runs it. A pipe or a redirection is
// therefore just a word; for those, name the shell yourself: --exec 'sh -c "..."'.
func SplitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("--exec: a ' is not closed")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New(`--exec: a " is not closed`)
			}
			inWord = true
		case c == '\\':
			if i+1 >= len(s) {
				return nil, errors.New(`--exec: a \ ends the command`)
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, errors.New("--exec is empty: give the command to run, e.g. --exec 'claude -p'")
	}
	return out, nil
}

// QuoteCommand shows argv so a person can read it back (for --dry-run and the log).
func QuoteCommand(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`*?;&|<>(){}[]#~") {
			q[i] = a
			continue
		}
		q[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(q, " ")
}

// Describe says in a few words how a run ended.
func (r Result) Describe(timeout time.Duration) string {
	took := r.Took.Round(time.Second)
	switch {
	case r.StartErr != nil:
		return fmt.Sprintf("could not start the command: %v", r.StartErr)
	case r.Stopped:
		return fmt.Sprintf("stopped after %s: the worker was shut down during the run", took)
	case r.TimedOut:
		return fmt.Sprintf("killed at the time limit (%s)", timeout)
	case r.ExitCode < 0:
		return fmt.Sprintf("killed by a signal after %s", took)
	}
	return fmt.Sprintf("exit %d after %s", r.ExitCode, took)
}
