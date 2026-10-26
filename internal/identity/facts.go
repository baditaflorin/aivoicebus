package identity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// The facts this machine advertises (see msg/facts.go), worked out here from an allowlist and
// nothing else:
//
//	signing-key   ~/.config/aivoicebus/release-signing.key is a regular file (stat only: never opened)
//	deploy-token  ~/.config/aivoicebus/deploy-token is a regular file (stat only: never opened)
//	fleet-guard   the version in ~/.fleet-guard/VERSION (its first 64 bytes, kept only if it is a version)
//	fleet-ctx     what `fleet-ctx --version` prints (kept only if it is a version)
//	ssh           which node ssh aliases resolve (`ssh -G` names another host): the aliases in
//	              AIVOICEBUS_FACT_SSH, else every Host alias ending in _bastion in ~/.ssh/config and
//	              the files it includes
//
// The probes start processes, and a hook runs on every prompt, so the answer is kept in
// ~/.config/aivoicebus/facts.json and worked out again at most every FactsTTL.
// AIVOICEBUS_FACTS=off advertises none.

// FactsTTL is how long worked-out facts are reused.
const FactsTTL = 5 * time.Minute

// maxSSHAliases is how many aliases are checked; a label holds about four anyway.
const maxSSHAliases = 8

// FactsCache is the file the facts are kept in between probes.
func FactsCache() string {
	if d := config.Dir(); d != "" {
		return filepath.Join(d, "facts.json")
	}
	return ""
}

// machineFacts is the cache: the two label values, when they were worked out, and the alias
// setting they were worked out with (a changed AIVOICEBUS_FACT_SSH is a reason to look again).
type machineFacts struct {
	At     time.Time `json:"at"`
	Has    string    `json:"has"`
	SSH    string    `json:"ssh,omitempty"`
	SSHEnv string    `json:"ssh_env,omitempty"`
}

// prober is everything working the facts out touches. A secret is only ever passed to stat:
// readHead is for the two files that hold no secret (fleet-guard's VERSION, the ssh config), and
// tests replace these to check exactly that.
type prober struct {
	home     string
	stat     func(path string) (os.FileInfo, error)
	readHead func(path string, n int64) ([]byte, error)
	run      func(name string, args ...string) ([]byte, error) // with a short timeout
	lookPath func(file string) (string, error)
	glob     func(pattern string) ([]string, error)
}

var (
	factsNow  = time.Now
	newProber = func() prober {
		h, _ := os.UserHomeDir()
		return prober{home: h, stat: os.Stat, readHead: readHead, run: runQuick, lookPath: exec.LookPath, glob: filepath.Glob}
	}
)

func readHead(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, n))
}

func runQuick(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = nil
	return cmd.Output()
}

// FactsOff reports whether this machine advertises no facts (AIVOICEBUS_FACTS=off).
func FactsOff() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("AIVOICEBUS_FACTS")))
	return v == "off" || v == "0" || v == "no"
}

// MachineFacts is this machine's has and ssh label values ("" for ssh when no alias resolves),
// from the cache when it is fresh, else worked out now and kept.
func MachineFacts() (has, ssh string) {
	env := strings.TrimSpace(os.Getenv("AIVOICEBUS_FACT_SSH"))
	path := FactsCache()
	now := factsNow()
	if c, ok := readFacts(path); ok && c.SSHEnv == env && !c.At.After(now) && now.Sub(c.At) < FactsTTL {
		return c.Has, c.SSH
	}
	p := newProber()
	c := machineFacts{At: now, SSHEnv: env}
	c.Has, c.SSH = p.facts(env)
	writeFacts(path, c)
	return c.Has, c.SSH
}

func readFacts(path string) (machineFacts, bool) {
	var c machineFacts
	if path == "" {
		return c, false
	}
	b, err := os.ReadFile(path)
	// whatever the file says goes out as labels: it must still read as facts
	if err != nil || json.Unmarshal(b, &c) != nil || !msg.ValidFacts(c.Has, c.SSH) {
		return c, false
	}
	return c, true
}

func writeFacts(path string, c machineFacts) {
	if path == "" {
		return
	}
	b, _ := json.Marshal(c)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".facts-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); werr != nil || cerr != nil || os.Rename(tmp.Name(), path) != nil {
		os.Remove(tmp.Name())
	}
}

// facts works the two label values out.
func (p prober) facts(sshEnv string) (has, ssh string) {
	if p.home == "" {
		return "", "" // no home, no allowlisted paths: nothing is said
	}
	var words []string
	cfg := filepath.Join(p.home, ".config", "aivoicebus")
	if p.isFile(filepath.Join(cfg, "release-signing.key")) {
		words = append(words, msg.FactSigningKey)
	}
	if p.isFile(filepath.Join(cfg, "deploy-token")) {
		words = append(words, msg.FactDeployToken)
	}
	if v := p.guardVersion(); v != "" {
		words = append(words, msg.FactFleetGuard+"-"+v)
	}
	if v := p.ctxVersion(); v != "" {
		words = append(words, msg.FactFleetCtx+"-"+v)
	}
	has = msg.FactValue(words)
	if aliases := p.resolving(p.sshCandidates(sshEnv)); len(aliases) > 0 {
		ssh = msg.FactValue(aliases)
	}
	return has, ssh
}

func (p prober) isFile(path string) bool {
	fi, err := p.stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func (p prober) guardVersion() string {
	b, err := p.readHead(filepath.Join(p.home, ".fleet-guard", "VERSION"), 64)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return msg.CleanVersion(line)
}

func (p prober) ctxVersion() string {
	bin, err := p.lookPath("fleet-ctx")
	if err != nil { // a hook's PATH may lack ~/.local/bin, where the installer puts it
		bin = filepath.Join(p.home, ".local", "bin", "fleet-ctx")
		if fi, err := p.stat(bin); err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			return ""
		}
	}
	out, err := p.run(bin, "--version")
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return msg.CleanVersion(line)
}

// sshCandidates is the aliases to check: those AIVOICEBUS_FACT_SSH names ("off" for none), else
// the Host aliases ending in _bastion in the ssh config. Sorted, each once, at most maxSSHAliases.
func (p prober) sshCandidates(env string) []string {
	var names []string
	switch {
	case strings.EqualFold(env, "off"):
		return nil
	case env != "":
		names = strings.FieldsFunc(env, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	default:
		seen := map[string]bool{}
		p.hostAliases(filepath.Join(p.home, ".ssh", "config"), 0, seen, func(a string) {
			if strings.HasSuffix(strings.ToLower(a), "_bastion") {
				names = append(names, a)
			}
		})
	}
	var out []string
	for _, n := range names {
		if msg.SSHWord(n) != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out[:min(len(out), maxSSHAliases)]
}

// hostAliases calls found with every name on a Host line of the ssh config at path and the files
// it includes, at most four deep and 32 files in all (a pattern or a negation is no alias, and
// sshCandidates drops it). Nothing else in those files is looked at.
func (p prober) hostAliases(path string, depth int, seen map[string]bool, found func(string)) {
	if depth > 4 || seen[path] || len(seen) >= 32 {
		return
	}
	seen[path] = true
	b, err := p.readHead(path, 1<<20)
	if err != nil {
		return
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		key, rest := sshDirective(sc.Text())
		switch key {
		case "host":
			for _, a := range strings.Fields(rest) {
				if a = strings.Trim(a, `"`); a != "" {
					found(a)
				}
			}
		case "include":
			for _, pat := range strings.Fields(rest) {
				pat = strings.Trim(pat, `"`)
				switch {
				case strings.HasPrefix(pat, "~/"):
					pat = filepath.Join(p.home, pat[2:])
				case !filepath.IsAbs(pat):
					pat = filepath.Join(p.home, ".ssh", pat)
				}
				files, _ := p.glob(pat)
				for _, f := range files {
					p.hostAliases(f, depth+1, seen, found)
				}
			}
		}
	}
}

// sshDirective splits an ssh config line into its lower-cased keyword and the rest ("Host=a b" too).
func sshDirective(line string) (string, string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", ""
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), ""
	}
	return strings.ToLower(line[:i]), strings.TrimLeft(line[i:], " \t=")
}

// resolving is the aliases `ssh -G` maps to another host name (one ssh does not know comes back
// as itself), as ssh label words, sorted. The checks run side by side.
func (p prober) resolving(aliases []string) []string {
	words := make([]string, len(aliases))
	var wg sync.WaitGroup
	for i, a := range aliases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := p.run("ssh", "-G", a)
			if err != nil {
				return
			}
			sc := bufio.NewScanner(bytes.NewReader(out))
			for sc.Scan() {
				if h, ok := strings.CutPrefix(sc.Text(), "hostname "); ok {
					if h = strings.TrimSpace(h); h != "" && !strings.EqualFold(h, a) {
						words[i] = msg.SSHWord(a)
					}
					return
				}
			}
		}()
	}
	wg.Wait()
	var out []string
	for _, w := range words {
		if w != "" && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	slices.Sort(out)
	return out
}
