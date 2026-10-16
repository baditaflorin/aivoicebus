package outbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// at makes the memory's clock say t for the rest of the test.
func at(t *testing.T, now time.Time) {
	t.Helper()
	old := clock
	clock = func() time.Time { return now }
	t.Cleanup(func() { clock = old })
}

var noon = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// mem points the process at an empty home and returns the path of the memory file.
func mem(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".config", "aivoicebus", sentFile)
}

func sentAs(from, to string) msg.Message { return msg.Message{ID: msg.NewID(), From: from, To: to} }

func TestSendingRemembersTheMessageWithoutItsBodyAndKeepsTheFilePrivate(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	path := filepath.Join(os.Getenv("HOME"), ".config", "aivoicebus", sentFile)
	p, err := Prepare(c, Request{To: reader1, Body: "a body that must stay out of the file"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Send(c)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("the file is private: %v", fi.Mode())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "must stay out") || strings.Contains(string(raw), "body") {
		t.Errorf("no body in the file: %s", raw)
	}
	var book map[string][]map[string]any
	if err := json.Unmarshal(raw, &book); err != nil {
		t.Fatal(err)
	}
	got := book["me"]
	if len(got) != 1 || got[0]["id"] != out.ID || got[0]["from"] != meID || got[0]["to"] != reader1 || len(got[0]) != 4 {
		t.Errorf("an id, a sender, a target and a time, keyed by machine: %s", raw)
	}
	if dir, _ := os.ReadDir(filepath.Dir(path)); len(dir) != 2 { // the memory and its lock; no temporary file
		t.Errorf("only the memory and its lock are left: %v", dir)
	}
}

func TestAnExistingMemoryFileIsReplacedAsPrivate(t *testing.T) {
	path := mem(t)
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte("{}"), 0o644)
	rememberSent(sentAs(meID, reader1))
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("%v %v", fi, err)
	}
}

func TestTheMemoryKeepsTheLastTenMessagesOfAMachineForADay(t *testing.T) {
	path := mem(t)
	var ids []string
	for i := 0; i < 12; i++ {
		at(t, noon.Add(time.Duration(i)*time.Minute))
		m := sentAs(meID, reader1)
		ids = append(ids, m.ID)
		rememberSent(m)
	}
	book := readSent(noon.Add(11 * time.Minute))
	if got := book["me"]; len(got) != sentKeep || got[0].ID != ids[2] || got[sentKeep-1].ID != ids[11] {
		t.Errorf("the last ten, oldest first: %+v", got)
	}
	// a second machine's messages are a separate list: they do not push the first one's out
	at(t, noon.Add(12*time.Minute))
	other := sentAs("air/codex.1", reader1)
	rememberSent(other)
	book = readSent(noon.Add(12 * time.Minute))
	if len(book["me"]) != sentKeep || len(book["air"]) != 1 {
		t.Errorf("per machine: %d %d", len(book["me"]), len(book["air"]))
	}
	// a day on, everything has been forgotten
	if _, _, ok := lastSent(meID, noon.Add(12*time.Minute+sentWindow+time.Second), false); ok {
		t.Error("after 24h nothing is offered")
	}
	// and the next send prunes the stale entries from the file itself
	at(t, noon.Add(48*time.Hour))
	fresh := sentAs(meID, reader1)
	rememberSent(fresh)
	raw, _ := os.ReadFile(path)
	var onDisk sentBook
	json.Unmarshal(raw, &onDisk)
	if len(onDisk["me"]) != 1 || onDisk["me"][0].ID != fresh.ID || len(onDisk["air"]) != 0 {
		t.Errorf("%s", raw)
	}
}

func TestSendingTheSameMessageTwiceRemembersItOnce(t *testing.T) {
	mem(t)
	at(t, noon)
	m := sentAs(meID, reader1)
	rememberSent(m)
	rememberSent(m)
	if got := readSent(noon)["me"]; len(got) != 1 {
		t.Errorf("%+v", got)
	}
}

func TestLastSentPrefersThisIdentityThenThisMachineAndNeverAnother(t *testing.T) {
	mem(t)
	at(t, noon)
	mine := sentAs(meID, reader1)
	rememberSent(mine)
	at(t, noon.Add(time.Minute))
	peer := sentAs(mePeer, reader1) // more recent, a session of this machine
	rememberSent(peer)
	at(t, noon.Add(2*time.Minute))
	rememberSent(sentAs(elsewho, reader1)) // another machine
	now := noon.Add(3 * time.Minute)

	if e, exact, ok := lastSent(meID, now, false); !ok || !exact || e.ID != mine.ID {
		t.Errorf("this exact identity first, though it is older: %+v %v", e, exact)
	}
	if e, exact, ok := lastSent(meID, now, true); !ok || exact || e.ID != peer.ID {
		t.Errorf("for the machine's latest, whoever sent it, and it says it was not this identity: %+v %v", e, exact)
	}
	third := "me/claude.0003"
	if e, exact, ok := lastSent(third, now, false); !ok || exact || e.ID != peer.ID {
		t.Errorf("else the latest of the machine, marked as not this identity's own: %+v %v", e, exact)
	}
	if e, _, ok := lastSent("imac/claude.1", now, false); ok {
		t.Errorf("a machine that sent nothing is offered nothing: %+v", e)
	}
	if e, exact, ok := lastSent("OTHER/claude.9", now, false); !ok || exact || e.From != elsewho {
		t.Errorf("the host is compared without case, the identity is not: %+v %v", e, exact)
	}
	if e, exact, ok := lastSent(elsewho, now, false); !ok || !exact || e.From != elsewho {
		t.Errorf("%+v %v", e, exact)
	}
}

func TestACorruptOrOddMemoryIsIgnoredNotTrusted(t *testing.T) {
	path := mem(t)
	os.MkdirAll(filepath.Dir(path), 0o700)
	at(t, noon)
	for name, content := range map[string]string{
		"garbage":                      "not json at all {",
		"wrong type":                   `["a","b"]`,
		"null":                         `null`,
		"empty":                        ``,
		"entries that are not entries": `{"me":[1,"x",null,{"id":"nope"}]}`,
	} {
		os.WriteFile(path, []byte(content), 0o600)
		if e, _, ok := lastSent(meID, noon, false); ok {
			t.Errorf("%s: %+v", name, e)
		}
	}
	good := sentAs(meID, reader1)
	rem := func(e msg.Message, from time.Time) string {
		b, _ := json.Marshal(sentBook{msg.HostOf(e.From): {{ID: e.ID, From: e.From, To: e.To, At: from}}})
		return string(b)
	}
	// each of these is shaped wrong for one reason, and is dropped for it
	upper := sentAs(meID, reader1)
	upper.ID = "ABCDEF0123" // upper-case hex: ToUpper of a random id is a no-op when it has no letter (about 1 run in 100)
	badFrom := sentAs("bad\x00/claude", reader1)
	badTo := sentAs(meID, "")
	wrongHost := sentAs("imac/claude.1", reader1) // filed under another machine below
	for name, content := range map[string]string{
		"an id that cannot be retracted": rem(upper, noon),
		"a sender with a control char":   rem(badFrom, noon),
		"no target":                      rem(badTo, noon),
		"too old":                        rem(good, noon.Add(-25*time.Hour)),
		"in the future":                  rem(good, noon.Add(time.Hour)),
		"filed under the wrong machine":  strings.Replace(rem(wrongHost, noon), `"imac"`, `"me"`, 1),
	} {
		os.WriteFile(path, []byte(content), 0o600)
		if e, _, ok := lastSent(meID, noon, false); ok {
			t.Errorf("%s: %+v", name, e)
		}
	}
	os.WriteFile(path, []byte(rem(good, noon.Add(-time.Hour))), 0o600)
	if e, exact, ok := lastSent(meID, noon, false); !ok || !exact || e.ID != good.ID {
		t.Errorf("a well-formed entry is used: %+v", e)
	}
	// a corrupt file is overwritten by the next send, which makes it good again
	os.WriteFile(path, []byte("garbage"), 0o600)
	rememberSent(good)
	if e, exact, ok := lastSent(meID, noon, false); !ok || !exact || e.ID != good.ID {
		t.Errorf("%+v", e)
	}
}

func TestAMemoryThatCannotBeWrittenIsNeverAnError(t *testing.T) {
	c, _ := retractNode(t, meID, nil)
	// ~/.config is a file, so the directory cannot be made
	home := os.Getenv("HOME")
	os.WriteFile(filepath.Join(home, ".config"), []byte("in the way"), 0o600)
	p, err := Prepare(c, Request{To: reader1, Body: "hello"}, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := p.Send(c); err != nil || out.ID == "" {
		t.Errorf("the send succeeds: %v %+v", err, out)
	}
	t.Setenv("HOME", "") // no home at all
	rememberSent(sentAs(meID, reader1))
	if _, _, ok := lastSent(meID, clock(), false); ok {
		t.Error("nothing to read without a home")
	}
}

func TestSentByIsOnlyWhatThisExactIdentitySent(t *testing.T) {
	mem(t)
	at(t, noon)
	mine, sibling, other := sentAs("air/codex.a1b2", "x/y"), sentAs("air/codex.c3d4", "x/y"), sentAs("lab/codex.a1b2", "x/y")
	for _, m := range []msg.Message{mine, sibling, other} {
		rememberSent(m)
	}
	got := SentBy("air/codex.a1b2")
	if len(got) != 1 || !got[mine.ID] {
		t.Errorf("a session on the same machine, or the same name elsewhere, is not this session: %v", got)
	}
	at(t, noon.Add(sentWindow+time.Minute))
	if got := SentBy("air/codex.a1b2"); len(got) != 0 {
		t.Errorf("what was sent more than a day ago is forgotten: %v", got)
	}
}
