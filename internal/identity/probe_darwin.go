//go:build darwin

package identity

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func platformModel() string {
	s, _ := syscall.Sysctl("hw.model")
	return s
}

func platformChip() string {
	s, _ := syscall.Sysctl("machdep.cpu.brand_string")
	return s
}

// platformProfile asks system_profiler for the marketing name ("MacBook Pro")
// and chip ("Apple M1 Pro"). It takes well under a second; the answer is cached.
func platformProfile() (name, chip string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/system_profiler", "SPHardwareDataType", "-json").Output()
	if err != nil {
		return "", "", false
	}
	var r struct {
		Hardware []struct {
			MachineName   string `json:"machine_name"`
			ChipType      string `json:"chip_type"`      // Apple silicon
			CPUType       string `json:"cpu_type"`       // Intel
			ProcessorName string `json:"processor_name"` // older Intel
		} `json:"SPHardwareDataType"`
	}
	if json.Unmarshal(out, &r) != nil || len(r.Hardware) == 0 {
		return "", "", false
	}
	h := r.Hardware[0]
	for _, c := range []string{h.ChipType, h.CPUType, h.ProcessorName} {
		if c != "" {
			chip = c
			break
		}
	}
	return h.MachineName, chip, h.MachineName != "" || chip != ""
}

// maxChain bounds the walk up the process tree.
const maxChain = 24

// platformChain lists the processes above this one, nearest first. One `ps` call reads the
// whole table (macOS has no /proc); any failure gives nil, never an error, so a machine where
// ps is blocked simply falls back to environment markers.
func platformChain() []Proc {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,lstart=,comm=").Output()
	if err != nil {
		return nil
	}
	rows := parsePS(string(out))
	var chain []Proc
	seen := map[int]bool{}
	for pid := os.Getppid(); pid > 1 && !seen[pid] && len(chain) < maxChain; {
		seen[pid] = true
		r, ok := rows[pid]
		if !ok {
			break
		}
		p := Proc{PID: pid, Start: r.start, Exe: r.exe}
		if interpreters[baseName(r.exe)] {
			p.Script = interpreterScript(ctx, pid, r.exe)
		}
		chain = append(chain, p)
		pid = r.ppid
	}
	return chain
}

// interpreterScript asks for one process's arguments, only for interpreters (a node script such
// as gemini is not told apart from any other node process by its executable).
func interpreterScript(ctx context.Context, pid int, exe string) string {
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	toks := strings.Fields(string(out))
	base := filepath.Base(exe)
	for i, t := range toks {
		if filepath.Base(t) == base {
			return scriptFromArgv(toks[i:])
		}
	}
	return ""
}
