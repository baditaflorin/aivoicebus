package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestURLsTrimsAndSkipsEmpties(t *testing.T) {
	t.Setenv("AIVOICEBUS_URL", " http://a:7777/ ,,http://b:7777 ,")
	want := []string{"http://a:7777", "http://b:7777"}
	if got := URLs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("URLs() = %v, want %v", got, want)
	}
	t.Setenv("AIVOICEBUS_URL", "")
	if got := URLs(); len(got) != 0 {
		t.Fatalf("empty env should give no URLs, got %v", got)
	}
}

func TestTokenPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	write := func(path, v string) {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(v+"\n"), 0o600)
	}
	write(filepath.Join(home, ".config/aivoicebus/token"), "from-default-file")
	explicit := filepath.Join(t.TempDir(), "explicit")
	write(explicit, "from-explicit-file")
	viaEnvFile := filepath.Join(t.TempDir(), "viaenv")
	write(viaEnvFile, "from-token-file-env")

	t.Setenv("AIVOICEBUS_TOKEN", "")
	t.Setenv("AIVOICEBUS_TOKEN_FILE", "")
	if got := Token(""); got != "from-default-file" {
		t.Errorf("default file: %q", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN_FILE", viaEnvFile)
	if got := Token(""); got != "from-token-file-env" {
		t.Errorf("AIVOICEBUS_TOKEN_FILE: %q", got)
	}
	if got := Token(explicit); got != "from-explicit-file" {
		t.Errorf("explicit file should beat AIVOICEBUS_TOKEN_FILE: %q", got)
	}
	t.Setenv("AIVOICEBUS_TOKEN", "from-env")
	if got := Token(explicit); got != "from-env" {
		t.Errorf("AIVOICEBUS_TOKEN should win: %q", got)
	}
}

func TestLoadEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	envFile := filepath.Join(home, ".config/aivoicebus/env")
	os.WriteFile(envFile, []byte("export PATH=\"$HOME/.local/bin:$PATH\"\nexport AIVOICEBUS_URL=\"http://a:7777,http://b:7777\"\n"), 0o644)

	t.Setenv("AIVOICEBUS_URL", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_URL"); got != "http://a:7777,http://b:7777" {
		t.Errorf("not loaded from the env file: %q", got)
	}
	t.Setenv("AIVOICEBUS_URL", "http://explicit:1")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_URL"); got != "http://explicit:1" {
		t.Errorf("an explicit AIVOICEBUS_URL must not be overridden: %q", got)
	}
}

func TestLoadEnvFileAlsoLoadsTheHostOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/env"),
		[]byte("export AIVOICEBUS_URL=http://a:7777\nexport AIVOICEBUS_HOST=laptop-a16\n"), 0o644)
	t.Setenv("AIVOICEBUS_URL", "")
	t.Setenv("AIVOICEBUS_HOST", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_HOST"); got != "laptop-a16" {
		t.Errorf("AIVOICEBUS_HOST = %q", got)
	}
	t.Setenv("AIVOICEBUS_HOST", "from-shell")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_HOST"); got != "from-shell" {
		t.Errorf("an explicit AIVOICEBUS_HOST must win: %q", got)
	}
}

func TestLoadEnvFileAlsoLoadsTheCapabilities(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".config/aivoicebus"), 0o700)
	os.WriteFile(filepath.Join(home, ".config/aivoicebus/env"), []byte("export AIVOICEBUS_CAPS=\"ask,run\"\n"), 0o644)
	t.Setenv("AIVOICEBUS_CAPS", "")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_CAPS"); got != "ask,run" {
		t.Errorf("AIVOICEBUS_CAPS = %q", got)
	}
	t.Setenv("AIVOICEBUS_CAPS", "read-only")
	LoadEnvFile()
	if got := os.Getenv("AIVOICEBUS_CAPS"); got != "read-only" {
		t.Errorf("an explicit value must win: %q", got)
	}
}

// learned saves a signed node list as discovery would, in a fresh home.
func learned(t *testing.T, d Discovered) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	if err := SaveDiscovered(d); err != nil {
		t.Fatal(err)
	}
}

func TestAConfiguredNodeTheListRetiresIsDroppedAndTheOthersFollow(t *testing.T) {
	gone := RetiredNode{Name: "old", URL: "http://c:7777", Since: time.Date(2026, 10, 21, 21, 36, 32, 0, time.UTC)}
	learned(t, Discovered{Serial: 2, Nodes: []string{"http://a:7777", "http://b:7777", "http://d:7777"}, Retired: []RetiredNode{gone, {Name: "x", URL: "http://x:7777"}}})
	t.Setenv("AIVOICEBUS_URL", "http://a:7777,http://c:7777/,http://b:7777")
	ns := Resolve()
	if want := []string{"http://a:7777", "http://b:7777", "http://d:7777"}; !reflect.DeepEqual(ns.URLs, want) {
		t.Errorf("URLs = %v, want %v", ns.URLs, want)
	}
	if !reflect.DeepEqual(ns.Retired, []RetiredNode{gone}) || ns.Kept {
		t.Errorf("only the configured retired node is reported, and it is dropped: %+v", ns)
	}
	if !reflect.DeepEqual(URLs(), ns.URLs) {
		t.Error("URLs is Resolve's URLs")
	}
}

func TestAListNeverTakesAwayEveryNodeAMachineHas(t *testing.T) {
	learned(t, Discovered{Serial: 2, Nodes: []string{"http://a:7777"}, Retired: []RetiredNode{{Name: "b", URL: "http://b:7777"}, {Name: "c", URL: "http://c:7777"}}})
	// every configured node retired, none of the list's: the list does not apply here
	t.Setenv("AIVOICEBUS_URL", "http://b:7777,http://c:7777")
	ns := Resolve()
	if want := []string{"http://b:7777", "http://c:7777"}; !reflect.DeepEqual(ns.URLs, want) || !ns.Kept || len(ns.Retired) != 2 {
		t.Errorf("kept, reported, nothing appended: %+v", ns)
	}
	// a retired node next to one the list does not know (a dev node): still not this list's machine
	t.Setenv("AIVOICEBUS_URL", "http://b:7777,http://dev:7777")
	if ns := Resolve(); !reflect.DeepEqual(ns.URLs, []string{"http://b:7777", "http://dev:7777"}) || !ns.Kept {
		t.Errorf("%+v", ns)
	}
	// a machine on another mesh: nothing retired, nothing appended
	t.Setenv("AIVOICEBUS_URL", "http://other:7777")
	if ns := Resolve(); !reflect.DeepEqual(ns.URLs, []string{"http://other:7777"}) || len(ns.Retired) != 0 || ns.Kept {
		t.Errorf("%+v", ns)
	}
}

func TestAnExpiredListRetiresNothing(t *testing.T) {
	learned(t, Discovered{Serial: 2, Nodes: []string{"http://a:7777", "http://b:7777"}, Retired: []RetiredNode{{Name: "c", URL: "http://c:7777"}}, ValidUntil: time.Now().Add(-time.Hour)})
	t.Setenv("AIVOICEBUS_URL", "http://a:7777,http://c:7777")
	if ns := Resolve(); !reflect.DeepEqual(ns.URLs, []string{"http://a:7777", "http://c:7777"}) || len(ns.Retired) != 0 {
		t.Errorf("%+v", ns)
	}
}

func TestALearnedNodeTheListAlsoRetiresIsNotAppended(t *testing.T) {
	// A list never holds both (ParseNodes refuses it), but the saved file is only a file.
	learned(t, Discovered{Serial: 2, Nodes: []string{"http://a:7777", "http://b:7777"}, Retired: []RetiredNode{{Name: "b", URL: "http://b:7777"}}})
	t.Setenv("AIVOICEBUS_URL", "http://a:7777")
	if ns := Resolve(); !reflect.DeepEqual(ns.URLs, []string{"http://a:7777"}) || len(ns.Retired) != 0 {
		t.Errorf("%+v", ns)
	}
	// and a configured node both listed and retired does not make the list apply
	t.Setenv("AIVOICEBUS_URL", "http://b:7777")
	if ns := Resolve(); !reflect.DeepEqual(ns.URLs, []string{"http://b:7777"}) || !ns.Kept {
		t.Errorf("%+v", ns)
	}
}

func TestAnOlderBinaryReadsTheSavedListWithRetiredNodes(t *testing.T) {
	learned(t, Discovered{Serial: 3, Nodes: []string{"http://a:7777"}, Retired: []RetiredNode{{Name: "b", URL: "http://b:7777"}}})
	b, err := os.ReadFile(filepath.Join(Dir(), "nodes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var old struct { // Discovered before retired nodes
		Serial int64    `json:"serial"`
		Nodes  []string `json:"nodes"`
	}
	if err := json.Unmarshal(b, &old); err != nil || old.Serial != 3 || len(old.Nodes) != 1 {
		t.Errorf("%v %+v\n%s", err, old, b)
	}
}
