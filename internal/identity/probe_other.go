//go:build !darwin

package identity

import (
	"os"
	"strconv"
	"strings"
)

// Servers and other non-Mac machines keep their plain hostname.

func platformModel() string { return "" }

func platformChip() string { return "" }

func platformProfile() (name, chip string, ok bool) { return "", "", false }

const maxChain = 24

// platformChain lists the processes above this one, nearest first, from /proc. Where there is
// no /proc (or it hides a process) the chain just ends: detection then uses the environment.
func platformChain() []Proc {
	var chain []Proc
	seen := map[int]bool{}
	for pid := os.Getppid(); pid > 1 && !seen[pid] && len(chain) < maxChain; {
		seen[pid] = true
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			break
		}
		// "pid (comm) state ppid ... starttime(field 22) ...": comm may hold spaces and
		// parentheses, so split after the last ")".
		s := string(raw)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			break
		}
		f := strings.Fields(s[i+1:])
		if len(f) < 20 {
			break
		}
		ppid, _ := strconv.Atoi(f[1])
		exe, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
		if err != nil {
			exe = s[strings.IndexByte(s, '(')+1 : i] // not ours to read: the short name will do
		}
		p := Proc{PID: pid, Start: f[19], Exe: exe}
		if interpreters[baseName(exe)] {
			if cmd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); err == nil {
				p.Script = scriptFromArgv(strings.Split(string(cmd), "\x00"))
			}
		}
		chain = append(chain, p)
		pid = ppid
	}
	return chain
}
