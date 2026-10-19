package cli

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/server"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// asCLI is this test binary run as the aivoicebus CLI (see TestMain) on a machine of its own: its
// own home, identity and nodes, and nothing of the caller's aivoicebus settings.
func asCLI(t *testing.T, home, me, urls string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AIVOICEBUS_") && !strings.HasPrefix(kv, "HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	pipe := os.Getenv("AIVOICEBUS_PIPE_URL") // off, unless the test runs a pipe of its own
	cmd.Env = append(cmd.Env, "AIVOICEBUS_TEST_AS_CLI=1", "HOME="+home, "AIVOICEBUS_ME="+me, "AIVOICEBUS_URL="+urls,
		"AIVOICEBUS_DETECT=off", "AIVOICEBUS_PIPE_URL="+pipe, "AIVOICEBUS_NO_CLAUDE_MCP=1")
	return cmd
}

// twoNodes starts nodes a and b, each replicating from the other, with the test token file.
func twoNodes(t *testing.T) (a, b string) {
	t.Helper()
	ts := tokenTokens(t)
	start := func(name string) (*store.Store, string) {
		s, _ := store.Open(t.TempDir(), name)
		srv := httptest.NewServer(server.NewHandler(s, thShared, server.WithNode(name), server.WithTokenSet(ts), server.WithObserverToken(thObs)))
		t.Cleanup(srv.Close)
		return s, srv.URL
	}
	sa, a := start("a")
	sb, b := start("b")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, p := range []struct {
		s    *store.Store
		peer string
	}{{sa, b}, {sb, a}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.Replicate(ctx, p.s, []string{p.peer}, thShared, 100*time.Millisecond, server.NewPeerBoard([]string{p.peer}))
		}()
	}
	t.Cleanup(func() { cancel(); wg.Wait() })
	return a, b
}

func writeSecret(t *testing.T, path, v string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole exchange, as it runs between two machines: the deploy machine on node a hands air,
// whose session reads from node b, its token, held by a node or by the pipe. Each side is its own
// process; this test is the person on air who approves, with the approver key.
func TestTokenHandOverBetweenTwoMachinesOnTwoNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("runs two CLI processes against two replicating nodes")
	}
	t.Run("held by a node", func(t *testing.T) { handOverTwoNodes(t, "node") })
	t.Run("held by the pipe", func(t *testing.T) {
		held, _ := services(t)
		handOverTwoNodes(t, "pipe")
		for _, env := range held() { // what is left of it there is the sealed envelope, never the token
			if strings.Contains(string(env), thAir) {
				t.Error("the pipe holds the token in the clear")
			}
		}
	})
}

// airTerm is the person's terminal on air: no agent tool above it, so it is the person's.
const airTerm = "air/agent"

func handOverTwoNodes(t *testing.T, via string) {
	a, b := twoNodes(t)
	deployHome, airHome := t.TempDir(), t.TempDir()
	writeSecret(t, filepath.Join(deployHome, ".config/aivoicebus/token"), thShared)
	writeSecret(t, filepath.Join(deployHome, ".config/aivoicebus/machine-tokens/air"), thAir)
	writeSecret(t, filepath.Join(airHome, ".config/aivoicebus/token"), thShared)
	t.Setenv("HOME", airHome) // the person's terminal on air
	t.Setenv("AIVOICEBUS_TOKEN", "")
	const pass = "the person on air knows this one"
	if _, err := config.CreateApprover(pass); err != nil {
		t.Fatal(err)
	}
	priv, err := config.OpenApprover(pass)
	if err != nil {
		t.Fatal(err)
	}

	// the deploy machine hands air its token
	var handOut, handErr strings.Builder
	hand := asCLI(t, deployHome, "deploy/claude.1", a+","+b, "token", "hand", "air", "--via", via, "--json", "--wait", "2m")
	hand.Stdout = &handOut
	pipe, _ := hand.StderrPipe()
	if err := hand.Start(); err != nil {
		t.Fatal(err)
	}
	taskID := make(chan string, 1)
	var errMu sync.Mutex
	go func() {
		sc := bufio.NewScanner(pipe)
		for sc.Scan() {
			line := sc.Text()
			errMu.Lock()
			handErr.WriteString(line + "\n")
			errMu.Unlock()
			if f := strings.Fields(line); len(f) > 2 && f[0] == "sent" && f[1] == "task" {
				taskID <- f[2]
			}
		}
	}()
	t.Cleanup(func() { _ = hand.Process.Kill() })
	var id string
	select {
	case id = <-taskID:
	case <-time.After(30 * time.Second):
		t.Fatal("hand sent no task")
	}

	// it went to the person on air, and an agent's session there may not take it
	ms, _ := (&client.Client{URLs: []string{a}, Token: thShared}).Thread(id)
	if len(ms) == 0 || ms[0].To != "air/human" || !msg.PersonOnly(ms[0]) {
		t.Fatalf("the hand task: %+v", ms)
	}
	agent := asCLI(t, airHome, "air/codex.1", b+","+a, "token", "install", id, "--wait", "1m")
	agent.Env = append(agent.Env, "AIVOICEBUS_AGENT=codex.1")
	if out, err := agent.CombinedOutput(); err == nil || !strings.Contains(string(out), "for the person on the machine, not for an agent") || !strings.Contains(string(out), "aivoicebus token install "+id) {
		t.Fatalf("an agent's install of the person's task: %v\n%s", err, out)
	}

	// the person on air runs install with the task's id, in their own terminal
	var instOut, instErr strings.Builder
	inst := asCLI(t, airHome, airTerm, b+","+a, "token", "install", id, "--json", "--wait", "2m")
	inst.Stdout, inst.Stderr = &instOut, &instErr
	if err := inst.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Process.Kill() })

	// the person approves at a terminal on air: a decision signed with the approver key, sent
	// (and signed with air's machine key) from air
	person := &client.Client{URLs: []string{b, a}, Token: thShared}
	var req msg.Message
	for deadline := time.Now().Add(60 * time.Second); req.ID == "" && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		ms, _ := person.Inbox("air/human", false)
		for _, m := range ms {
			if strings.HasPrefix(m.Body, "[approval needed]") && m.From == airTerm {
				req = m
			}
		}
	}
	if req.ID == "" {
		t.Fatalf("no approval request\ninstall: %s", instErr.String())
	}
	sig := ed25519.Sign(priv, msg.ApprovalStatement(req.ID, "approve", req.From))
	if _, err := person.Send(msg.Message{ID: msg.NewID(), From: "air/human", To: req.From, Re: req.ID, Thread: req.Thread, Body: msg.ApprovalBody("approve", sig)}); err != nil {
		t.Fatal(err)
	}

	waitFor := func(cmd *exec.Cmd, what string) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				errMu.Lock()
				defer errMu.Unlock()
				t.Fatalf("%s: %v\nhand: %s%s\ninstall: %s%s", what, err, handOut.String(), handErr.String(), instOut.String(), instErr.String())
			}
		case <-time.After(90 * time.Second):
			t.Fatalf("%s did not finish", what)
		}
	}
	waitFor(inst, "install")
	waitFor(hand, "hand")

	var ir installResult
	if err := json.Unmarshal([]byte(instOut.String()), &ir); err != nil || !ir.Installed || ir.Kind != "machine" || ir.Name != "air" || ir.Previous != "shared" || ir.Approved != "approver key" || ir.Task != id || ir.From != "deploy/claude.1" {
		t.Errorf("install: %+v %v", ir, err)
	}
	var hr handResult
	if err := json.Unmarshal([]byte(handOut.String()), &hr); err != nil || !hr.Installed || !hr.Sealed || !hr.Proven || hr.Session != airTerm || hr.Approved != "approver key" || hr.Answer != ir.Answer || !strings.Contains(hr.Proof, "saw its answer "+ir.Answer+" sent with the machine token \"air\"") {
		t.Errorf("hand: %+v %v", hr, err)
	}
	// while the person had not yet approved, hand said what it waited for
	errMu.Lock()
	said := handErr.String()
	errMu.Unlock()
	if !strings.Contains(said, "taken by "+airTerm+", waiting for the person's approval on air (approve "+req.ID+" there") || hr.Approval != req.ID {
		t.Errorf("hand did not show the wait for approval %s: %q %+v", req.ID, said, hr)
	}
	if got, _ := os.ReadFile(filepath.Join(airHome, ".config/aivoicebus/token")); strings.TrimSpace(string(got)) != thAir {
		t.Error("air's token file does not hold its machine token")
	}

	// The token is in no message on either node, in no output, and in no file but the two it
	// belongs in (the deploy machine's machine-tokens/air and air's token file).
	for _, u := range []string{a, b} {
		recs, err := (&client.Client{URLs: []string{u}, Token: thShared}).Sync(map[string]uint64{})
		raw, _ := json.Marshal(recs)
		if err != nil || len(recs) < 6 || strings.Contains(string(raw), thAir) {
			t.Errorf("node %s: %d records, holds the token: %v (%v)", u, len(recs), strings.Contains(string(raw), thAir), err)
		}
		want := "[attach] " + a + "/v1/files/"
		if via == "pipe" {
			want = "[attach] " + os.Getenv("AIVOICEBUS_PIPE_URL") + "/#"
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("node %s: no link message to %s (the exchange went some other way)", u, want)
		}
		// the link's key is sealed for air's machine key: no message holds a key that opens it
		if reKeyInLink.MatchString(string(raw)) || !strings.Contains(string(raw), "sealed=v1.") {
			t.Errorf("node %s: the link message carries its key, or no seal", u)
		}
	}
	errMu.Lock()
	outputs := handOut.String() + handErr.String() + instOut.String() + instErr.String()
	errMu.Unlock()
	if strings.Contains(outputs, thAir) || strings.Contains(outputs, thShared) || strings.Contains(outputs, "/v1/files/") || strings.Contains(outputs, "~") {
		t.Error("a token or the link was printed")
	}
	allowed := map[string]bool{
		filepath.Join(deployHome, ".config/aivoicebus/machine-tokens/air"): true,
		filepath.Join(airHome, ".config/aivoicebus/token"):                 true,
	}
	for _, home := range []string{deployHome, airHome} {
		_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || allowed[p] {
				return nil
			}
			if b, _ := os.ReadFile(p); strings.Contains(string(b), thAir) {
				t.Errorf("%s holds the token", p)
			}
			return nil
		})
	}
}
