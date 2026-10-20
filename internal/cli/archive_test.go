package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestExportUnionsAllNodesAndNeverReplacesAGoodBackupWithAFailedOne(t *testing.T) {
	stores := nodes(t, 2, nil)
	session(t, "a/claude.1", "")
	stores[0].Send(msg.Message{From: "a/claude.1", To: "b/x", Body: "only on node 0", Prio: msg.PrioFYI})
	stores[1].Send(msg.Message{From: "a/claude.1", To: "b/x", Body: "only on node 1", Prio: msg.PrioFYI})

	file := filepath.Join(t.TempDir(), "board.jsonl")
	if code, _, e := run(t, "", "export", "--out", file); code != 0 || !strings.Contains(e, "exported 2 record(s) from 2 origin(s)") {
		t.Fatalf("each node holds one record the other lacks, and the export has both: %d %q", code, e)
	}
	good, _ := os.ReadFile(file)

	t.Setenv("AIVOICEBUS_URL", "http://127.0.0.1:1") // nothing there
	if code, _, e := run(t, "", "export", "--out", file); code != 1 || !strings.Contains(e, "no node reachable") {
		t.Fatalf("%d %q", code, e)
	}
	if now, _ := os.ReadFile(file); string(now) != string(good) {
		t.Error("a failed export must leave the earlier backup as it was")
	}
	if _, err := os.Stat(file + ".partial"); err == nil {
		t.Error("and clean up after itself")
	}
}

func TestAnImportRunAsRootGivesTheFilesToTheDirectorysOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only meaningful as root, which is how an operator runs it against a service's state directory")
	}
	stores := nodes(t, 1, nil)
	session(t, "a/claude.1", "")
	stores[0].Send(msg.Message{From: "a/claude.1", To: "b/x", Body: "one", Prio: msg.PrioFYI})
	file := filepath.Join(t.TempDir(), "board.jsonl")
	if code, _, e := run(t, "", "export", "--out", file); code != 0 {
		t.Fatal(e)
	}
	dir := t.TempDir()
	if err := os.Chown(dir, 23456, 23456); err != nil {
		t.Skip("cannot chown here:", err)
	}
	if code, _, e := run(t, "", "import", "--data", dir, "--node", "n9", file); code != 0 {
		t.Fatal(e)
	}
	fi, err := os.Stat(filepath.Join(dir, "log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if st := fi.Sys().(*syscall.Stat_t); st.Uid != 23456 || st.Gid != 23456 {
		t.Errorf("the service's dynamic user must be able to read what was imported: owner %d:%d", st.Uid, st.Gid)
	}
}

func TestExportThenImportRestoresAWholeBoardIntoAFreshNode(t *testing.T) {
	stores := nodes(t, 1, nil)
	session(t, "a/claude.1", "")
	for i := 0; i < 7; i++ {
		run(t, "", "send", "--to", "b/codex.2", "--queue", "--prio", "fyi", strings.Repeat("x", i+1))
	}
	session(t, "b/codex.2", "")
	_, out, _ := run(t, "", "inbox", "--ack")
	if !strings.Contains(out, "xxxxxxx") {
		t.Fatal(out)
	}
	want := stores[0].VV()

	file := filepath.Join(t.TempDir(), "board.jsonl")
	code, _, e := run(t, "", "export", "--out", file)
	if code != 0 || !strings.Contains(e, "exported 14 record(s) from 1 origin(s)") {
		t.Fatalf("%d %q", code, e)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("an export holds every message: mode %v", fi.Mode().Perm())
	}

	dir := t.TempDir()
	code, out, e = run(t, "", "import", "--data", dir, "--node", "n9", file)
	if code != 0 || !strings.Contains(out, "read 14 record(s): 14 added, 0 already there") {
		t.Fatalf("%d %q %q", code, out, e)
	}
	restored, err := store.Open(dir, "n9")
	if err != nil {
		t.Fatal(err)
	}
	got := restored.VV()
	if len(got) != len(want) || got["n0"] != want["n0"] {
		t.Errorf("version vector %v, want %v", got, want)
	}
	if left := restored.Inbox("b/codex.2", false); len(left) != 0 {
		t.Errorf("acknowledgements come back too: %+v", left)
	}
	if all := restored.Inbox("b/codex.2", true); len(all) != 7 {
		t.Errorf("every message comes back: %d", len(all))
	}

	_, out, _ = run(t, "", "import", "--data", dir, "--node", "n9", file)
	if !strings.Contains(out, "read 14 record(s): 0 added, 14 already there") || strings.Contains(out, "gap") {
		t.Errorf("importing twice adds nothing: %q", out)
	}
}

func TestImportRefusesWhatIsNotAnExportAndReportsGaps(t *testing.T) {
	mesh(t)
	dir := t.TempDir()
	bad := filepath.Join(t.TempDir(), "bad.jsonl")
	os.WriteFile(bad, []byte(`{"origin":"n1","seq":1,"msg":{"id":"0123456789","from":"a/b","to":"all","thread":"0123456789","body":"hi","at":"2026-10-14T00:00:00Z"}}`+"\nnot json\n"), 0o600)
	if code, _, e := run(t, "", "import", "--data", dir, bad); code != 1 || !strings.Contains(e, "line 2 is not a record") {
		t.Errorf("%d %q", code, e)
	}
	if code, _, e := run(t, "", "import", bad); code != 1 || !strings.Contains(e, "usage: import") {
		t.Errorf("%d %q", code, e)
	}

	gap := filepath.Join(t.TempDir(), "gap.jsonl")
	r := func(seq string) string {
		return `{"origin":"n1","seq":` + seq + `,"msg":{"id":"01234567` + "0" + seq + `","from":"a/b","to":"all","thread":"x","body":"hi","at":"2026-10-14T00:00:00Z"}}` + "\n"
	}
	os.WriteFile(gap, []byte(r("1")+r("3")), 0o600) // 2 is missing
	_, out, _ := run(t, "", "import", "--data", dir, gap)
	if !strings.Contains(out, "1 added") || !strings.Contains(out, "1 out of order or after a gap") {
		t.Errorf("%q", out)
	}
}
