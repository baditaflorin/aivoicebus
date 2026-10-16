//go:build unix

package outbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// patient makes remembering wait long enough that a busy machine cannot make a test skip an entry:
// the 200 ms of production is for not holding a send up, not for these tests to race.
func patient(t *testing.T) {
	t.Helper()
	old := sentLockWait
	sentLockWait = time.Minute
	t.Cleanup(func() { sentLockWait = old })
}

// countsByHost reads what the memory holds, per machine.
func countsByHost(t *testing.T) (counts map[string]int, total int) {
	t.Helper()
	counts = map[string]int{}
	for host, list := range readSent(clock()) {
		counts[host] = len(list)
		total += len(list)
	}
	return counts, total
}

func TestConcurrentSendsLoseNoRememberedEntry(t *testing.T) {
	mem(t)
	patient(t)
	const hosts, perHost = 24, 8 // at most sentKeep per machine are kept, so no machine gets more than that
	var wg sync.WaitGroup
	start := make(chan struct{})
	for h := 0; h < hosts; h++ {
		for i := 0; i < perHost; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rememberSent(sentAs(fmt.Sprintf("host%02d/claude.%d", h, i), "all"))
			}()
		}
	}
	close(start)
	wg.Wait()
	counts, total := countsByHost(t)
	if total != hosts*perHost || len(counts) != hosts {
		t.Fatalf("every send is remembered: %d of %d entries on %d of %d machines: %v", total, hosts*perHost, len(counts), hosts, counts)
	}
}

// The child of TestSeveralProcessesLoseNoRememberedEntry: it is this test binary run again, told by
// the environment to remember messages as one machine and nothing else.
func TestSentWriterProcess(t *testing.T) {
	host := os.Getenv("AIVOICEBUS_TEST_SENT_WRITER")
	if host == "" {
		t.Skip("run by TestSeveralProcessesLoseNoRememberedEntry")
	}
	patient(t)
	for i := 0; i < perProcess; i++ {
		rememberSent(sentAs(fmt.Sprintf("%s/claude.%d", host, i), "all"))
	}
}

const perProcess = 8

func TestSeveralProcessesLoseNoRememberedEntry(t *testing.T) {
	path := mem(t)
	home := filepath.Dir(filepath.Dir(filepath.Dir(path)))
	const procs = 12
	var wg sync.WaitGroup
	errs := make(chan error, procs)
	for p := 0; p < procs; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestSentWriterProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "HOME="+home, fmt.Sprintf("AIVOICEBUS_TEST_SENT_WRITER=proc%02d", p))
			if out, err := cmd.CombinedOutput(); err != nil {
				errs <- fmt.Errorf("process %d: %v\n%s", p, err, out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	t.Setenv("HOME", home)
	counts, total := countsByHost(t)
	if total != procs*perProcess || len(counts) != procs {
		t.Fatalf("every send is remembered, from every process: %d of %d entries on %d of %d machines: %v", total, procs*perProcess, len(counts), procs, counts)
	}
}

func TestASendIsNeverHeldUpByAnotherThatHoldsTheLock(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	dir := filepath.Join(os.Getenv("HOME"), ".config", "aivoicebus")
	os.MkdirAll(dir, 0o700)
	unlock, ok := lockSent(filepath.Join(dir, sentLockFile), time.Second)
	if !ok {
		t.Fatal("the lock is free")
	}
	old := sentLockWait
	sentLockWait = 50 * time.Millisecond
	t.Cleanup(func() { sentLockWait = old })

	p, err := Prepare(c, Request{To: reader1, Body: "sent while another send was remembering"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	out, err := p.Send(c)
	if err != nil || out.ID == "" {
		t.Fatalf("the send succeeds: %v %+v", err, out)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Errorf("remembering gives up quickly instead of holding the send up: %v", waited)
	}
	if _, _, found := lastSent(meID, clock(), false); found {
		t.Error("while another send holds the lock this one is not remembered: best effort stays best effort")
	}
	unlock()
	rememberSent(out)
	if e, _, found := lastSent(meID, clock(), false); !found || e.ID != out.ID {
		t.Errorf("once it is free it is remembered: %+v %v", e, found)
	}
}

func TestTheLockIsNotLeftHeldAfterRemembering(t *testing.T) {
	path := mem(t)
	rememberSent(sentAs(meID, reader1))
	unlock, ok := lockSent(filepath.Join(filepath.Dir(path), sentLockFile), 50*time.Millisecond)
	if !ok {
		t.Fatal("remembering released the lock")
	}
	unlock()
	if fi, err := os.Stat(filepath.Join(filepath.Dir(path), sentLockFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the lock file is private: %v %v", fi, err)
	}
}
