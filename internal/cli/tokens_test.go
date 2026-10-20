package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/hook"
	"github.com/baditaflorin/aivoicebus/internal/worker"
)

// tokens adds up what the hooks, MCP and an agent's CLI reads put into each session (counts only,
// never the text), and what the worker's runs reported.
func TestTokensReportsContextAndWorkerRuns(t *testing.T) {
	mesh(t)
	t.Setenv("AIVOICEBUS_ME", boss)
	run(t, "", "send", "--to", sa, "--queue", "a fairly long message so the hook has something to show "+strings.Repeat("x", 400))
	t.Setenv("AIVOICEBUS_ME", sa)
	var out strings.Builder
	hook.Run([]string{"--tool", "claude"}, strings.NewReader(`{"session_id":"s1"}`), &out)
	if out.Len() == 0 {
		t.Fatal("the hook showed nothing")
	}
	t.Setenv("AIVOICEBUS_AGENT", "claude.a1") // an agent's own CLI read counts too
	run(t, "", "inbox", "--all")
	b, _ := os.ReadFile(config.Dir() + "/usage.jsonl")
	if strings.Contains(string(b), "fairly long message") {
		t.Fatal("the usage log holds the text")
	}
	lines := config.ReadUsage(time.Time{})
	if len(lines) < 2 {
		t.Fatalf("usage lines: %+v", lines)
	}
	l, _ := worker.FileLog(worker.LogPath())
	l(worker.Entry{At: time.Now().UTC(), Task: "t000000001", From: boss, Outcome: "ran", Usage: &worker.Usage{Input: 1000, Cached: 600, Output: 20}})
	code, js, e := run(t, "", "tokens", "--json")
	var v tokensView
	if err := json.Unmarshal([]byte(js), &v); err != nil || code != 0 {
		t.Fatalf("%d %v %s %s", code, err, js, e)
	}
	if len(v.Sessions) != 1 || v.Sessions[0].Me != sa || v.Sessions[0].HookChars == 0 || v.Sessions[0].CLIChars == 0 || v.Context == 0 {
		t.Errorf("sessions: %+v", v.Sessions)
	}
	if v.Worker.Runs != 1 || v.Worker.TokensIn != 1000 || v.Worker.Cached != 600 || v.Worker.TokensOut != 20 {
		t.Errorf("worker: %+v", v.Worker)
	}
	if _, text, _ := run(t, "", "tokens"); !strings.Contains(text, "worker runs: 1 (1 reported counts): 1000 tokens in (600 cached), 20 out") {
		t.Errorf("text: %s", text)
	}
}
