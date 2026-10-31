package worker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// Named commands. A task whose body starts with RunTag asks the worker to run one command the owner
// of this machine named in advance (a file in run.d/), with arguments that match the patterns that
// file gives. No model is involved and no shell runs: the worker looks the name up, checks every
// argument, and starts the argv the file spells out. A name that is not in run.d/ is refused, so a
// peer cannot make this machine run anything the owner did not write down.

// RunTag starts the body of a task that asks for a named command.
const RunTag = "[run] "

// MaxRunTimeout bounds what a spec may ask for, whatever its file says.
const MaxRunTimeout = 10 * time.Minute

// RunRequest is what a [run] task carries, as JSON after RunTag.
type RunRequest struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args,omitempty"`
}

// EncodeRun is the body of a task asking for the named command with args.
func EncodeRun(cmd string, args []string) (string, error) {
	if !nameRE.MatchString(cmd) {
		return "", fmt.Errorf("%q is not a command name (lowercase letters, digits, - and _, at most 32)", cmd)
	}
	b, err := json.Marshal(RunRequest{Cmd: cmd, Args: args})
	if err != nil {
		return "", err
	}
	return RunTag + string(b), nil
}

// IsRun reports whether a task body asks for a named command.
func IsRun(body string) bool { return strings.HasPrefix(body, RunTag) }

// ParseRun reads a [run] body. The JSON must be exactly a RunRequest: an unknown field is refused.
func ParseRun(body string) (RunRequest, error) {
	var r RunRequest
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(body, RunTag)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("the [run] body is not {\"cmd\":NAME,\"args\":[...]}: %v", err)
	}
	if dec.More() {
		return r, errors.New("the [run] body has text after its JSON")
	}
	if !nameRE.MatchString(r.Cmd) {
		return r, fmt.Errorf("%q is not a command name", r.Cmd)
	}
	return r, nil
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Spec is one named command, the content of run.d/NAME.json.
//
//	{
//	  "argv":    ["/usr/bin/python3", "-I", "/Users/me/bin/bright.py", "{0}"],
//	  "args":    ["^(0(\\.[0-9]+)?|1(\\.0+)?)$"],
//	  "sandbox": false,
//	  "timeout": "30s"
//	}
//
// argv[0] is an absolute path (nothing is looked up on PATH). "{N}" inside an argv word is replaced
// by the Nth argument. args has one pattern per argument; a request must give exactly that many, and
// each must match its pattern in full. sandbox defaults to true (see Sandboxed); a command that
// cannot work inside it (display settings, for one) says false, for itself only.
type Spec struct {
	Argv    []string `json:"argv"`
	Args    []string `json:"args"`
	Sandbox *bool    `json:"sandbox,omitempty"`
	Timeout string   `json:"timeout,omitempty"`

	name string
	pats []*regexp.Regexp
	to   time.Duration
}

// RunSet is the named commands of one machine.
type RunSet struct {
	Dir   string
	Specs map[string]*Spec
}

// RunDir is where the named commands live (~/.config/aivoicebus/run.d).
func RunDir() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, "run.d")
	}
	return ""
}

// LoadRunSet reads every NAME.json in dir. A file that does not parse, or that breaks a rule,
// fails the whole load: a half-read set would run less than the owner wrote down, or more.
func LoadRunSet(dir string) (*RunSet, error) {
	if dir == "" {
		return nil, errors.New("no home directory for run.d")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("no named commands: %v (write one as %s/NAME.json, see `aivoicebus worker --help`)", err, dir)
	}
	rs := &RunSet{Dir: dir, Specs: map[string]*Spec{}}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		name := strings.TrimSuffix(n, ".json")
		path := filepath.Join(dir, n)
		fi, err := os.Lstat(path)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file (a symlink is refused)", path)
		}
		if fi.Mode().Perm()&0o022 != 0 {
			return nil, fmt.Errorf("%s is writable by group or others: chmod go-w it (whoever can write it can run a command here)", path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sp, err := parseSpec(name, b)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		rs.Specs[name] = sp
	}
	if len(rs.Specs) == 0 {
		return nil, fmt.Errorf("%s holds no NAME.json: write at least one named command first", dir)
	}
	return rs, nil
}

func parseSpec(name string, b []byte) (*Spec, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("%q is not a command name (lowercase letters, digits, - and _, at most 32)", name)
	}
	var sp Spec
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sp); err != nil {
		return nil, err
	}
	sp.name = name
	if len(sp.Argv) == 0 || !filepath.IsAbs(sp.Argv[0]) {
		return nil, errors.New("argv[0] must be an absolute path")
	}
	for _, p := range sp.Args {
		re, err := regexp.Compile("^(?:" + strings.TrimSuffix(strings.TrimPrefix(p, "^"), "$") + ")$")
		if err != nil {
			return nil, fmt.Errorf("args pattern %q: %v", p, err)
		}
		sp.pats = append(sp.pats, re)
	}
	for _, w := range sp.Argv {
		for _, m := range placeholderRE.FindAllStringSubmatch(w, -1) {
			var i int
			fmt.Sscanf(m[1], "%d", &i)
			if i >= len(sp.Args) {
				return nil, fmt.Errorf("argv uses {%d} but args has %d pattern(s)", i, len(sp.Args))
			}
		}
	}
	sp.to = time.Minute
	if sp.Timeout != "" {
		d, err := time.ParseDuration(sp.Timeout)
		if err != nil || d < time.Second {
			return nil, fmt.Errorf("timeout %q: a duration of at least 1s", sp.Timeout)
		}
		sp.to = min(d, MaxRunTimeout)
	}
	return &sp, nil
}

var placeholderRE = regexp.MustCompile(`\{([0-9]+)\}`)

// UseSandbox says whether the command runs inside the secrets sandbox.
func (sp *Spec) UseSandbox() bool { return sp.Sandbox == nil || *sp.Sandbox }

// Limit is how long the command may run.
func (sp *Spec) Limit() time.Duration { return sp.to }

// Names lists the named commands, sorted.
func (rs *RunSet) Names() []string {
	var out []string
	for n := range rs.Specs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Resolve checks a request against the set and returns the argv to start.
func (rs *RunSet) Resolve(r RunRequest) (argv []string, sp *Spec, err error) {
	sp = rs.Specs[r.Cmd]
	if sp == nil {
		return nil, nil, fmt.Errorf("no named command %q on this machine (it has: %s)", r.Cmd, strings.Join(rs.Names(), ", "))
	}
	if len(r.Args) != len(sp.pats) {
		return nil, sp, fmt.Errorf("%s takes %d argument(s), the request has %d", r.Cmd, len(sp.pats), len(r.Args))
	}
	for i, a := range r.Args {
		if !sp.pats[i].MatchString(a) {
			return nil, sp, fmt.Errorf("argument %d of %s does not match the pattern its file allows", i, r.Cmd)
		}
	}
	for _, w := range sp.Argv {
		argv = append(argv, placeholderRE.ReplaceAllStringFunc(w, func(m string) string {
			var i int
			fmt.Sscanf(m, "{%d}", &i)
			return r.Args[i]
		}))
	}
	return argv, sp, nil
}
