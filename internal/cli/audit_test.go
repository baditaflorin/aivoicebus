package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
)

// The audit log records what the person and the worker did; --verify catches an edited line.
func TestTheAuditLogRecordsAndCatchesAnEdit(t *testing.T) {
	mark := workerSetup(t, "desk")
	sendTask(t, boss, "lap", "write the mark")
	runWorker(t, "--exec", "touch "+mark)
	t.Setenv("AIVOICEBUS_ME", boss)
	run(t, "", "worker", "allow", "add", "box")
	run(t, "", "pause", "lap", "--for", "5m", "testing the audit")
	_, out, _ := run(t, "", "audit")
	for _, want := range []string{"worker allow add", "worker ran", "pause"} {
		if !strings.Contains(out, want) {
			t.Errorf("audit lacks %q:\n%s", want, out)
		}
	}
	if code, v, _ := run(t, "", "audit", "--verify"); code != 0 || !strings.Contains(v, "audit log holds") {
		t.Fatalf("verify: %d %s", code, v)
	}
	p := config.Dir() + "/audit.jsonl"
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "testing the audit", "nothing to see", 1)), 0o600)
	if code, v, _ := run(t, "", "audit", "--verify"); code == 0 || !strings.Contains(v, "signature") {
		t.Errorf("an edited line passed: %d %s", code, v)
	}
}
