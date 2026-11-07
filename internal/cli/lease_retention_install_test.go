package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func retentionHelper(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join("..", "..", "deploy", "lease-retention.sh")}, args...)...)
	b, e := cmd.CombinedOutput()
	return string(b), e
}
func TestLeaseRetentionInstallerCanonicalConfiguration(t *testing.T) {
	for _, value := range []string{"", "--lease-retention off", "--lease-retention observe", "--lease-retention observe --lease-retention-hosts builder-1", "--lease-retention on --lease-retention-hosts builder-1,builder_2"} {
		if b, e := retentionHelper(t, "validate", value); e != nil {
			t.Fatalf("valid %q refused: %v %s", value, e, b)
		}
	}
	for _, value := range []string{"--lease-retention on", "--lease-retention on --lease-retention-hosts a,a", "--lease-retention on --lease-retention-hosts a,", "--lease-retention on --lease-retention-hosts *", "--lease-retention on --lease-retention-hosts a --addr 0.0.0.0:22", "--lease-retention observe; echo injected", "--lease-retention observe\nLEASE_RETENTION_ARGS=", "\"--lease-retention observe\"", "--lease-retention on --lease-retention-hosts $(whoami)"} {
		if b, e := retentionHelper(t, "validate", value); e == nil {
			t.Fatalf("invalid %q accepted: %s", value, b)
		}
	}
	dir := t.TempDir()
	env := filepath.Join(dir, "node.env")
	for _, text := range []string{"NODE=fixture\n", "LEASE_RETENTION_ARGS=--lease-retention observe\n", "NODE=fixture\nLEASE_RETENTION_ARGS=--lease-retention on --lease-retention-hosts builder-1\n"} {
		if e := os.WriteFile(env, []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
		out, e := retentionHelper(t, "check-env", env)
		if e != nil {
			t.Fatal(e, out)
		}
		want := ""
		for _, line := range strings.Split(text, "\n") {
			if strings.HasPrefix(line, "LEASE_RETENTION_ARGS=") {
				want = line + "\n"
			}
		}
		if out != want {
			t.Fatalf("preserved output %q want %q", out, want)
		}
		after, _ := os.ReadFile(env)
		if string(after) != text {
			t.Fatal("read-only preflight mutated config")
		}
	}
	for _, text := range []string{"LEASE_RETENTION_ARGS=\nLEASE_RETENTION_ARGS=--lease-retention observe\n", " LEASE_RETENTION_ARGS=--lease-retention observe\n", "LEASE_RETENTION_ARGS =--lease-retention observe\n", "LEASE_RETENTION_ARGS=--lease-retention on --lease-retention-hosts a;touch injected\n"} {
		if e := os.WriteFile(env, []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
		if b, e := retentionHelper(t, "check-env", env); e == nil {
			t.Fatalf("invalid config accepted: %q %s", text, b)
		}
	}
	link := filepath.Join(dir, "linked")
	if e := os.Symlink(env, link); e != nil {
		t.Fatal(e)
	}
	if b, e := retentionHelper(t, "check-env", link); e == nil {
		t.Fatal("config alias accepted", b)
	}
}
func TestLeaseRetentionInvalidInstallerInputPrecedesCredentialWrite(t *testing.T) {
	root := t.TempDir()
	token := filepath.Join(root, "token")
	nodes := filepath.Join(root, "nodes.conf")
	if e := os.WriteFile(nodes, []byte("fixture:fixture:127.0.0.1\n"), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("bash", filepath.Join("..", "..", "deploy", "install.sh"), "--gen-token")
	cmd.Env = append(os.Environ(), "HOME="+root, "AIVOICEBUS_NODES_CONF="+nodes, "AIVOICEBUS_TOKEN_FILE="+token, "AIVOICEBUS_NODE_LEASE_RETENTION_ARGS=--lease-retention on --addr 0.0.0.0:22")
	b, e := cmd.CombinedOutput()
	if e == nil {
		t.Fatal("invalid input proceeded", string(b))
	}
	if _, e = os.Stat(token); !os.IsNotExist(e) {
		t.Fatalf("credential written before validation: %v %s", e, b)
	}
}
func TestLeaseRetentionInvalidRetainedConfigPrecedesRemoteWrites(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if e := os.Mkdir(bin, 0700); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(root, "write-called")
	nodeEnv := filepath.Join(root, "node.env")
	token := filepath.Join(root, "token")
	nodes := filepath.Join(root, "nodes.conf")
	for p, text := range map[string]string{nodeEnv: "LEASE_RETENTION_ARGS=\nLEASE_RETENTION_ARGS=--lease-retention observe\n", token: "synthetic-token-only-for-installer-tests-0000000000", nodes: "fixture:fixture:127.0.0.1\n"} {
		if e := os.WriteFile(p, []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
	}
	ssh := `#!/bin/bash
while [ $# -gt 0 ]; do case "$1" in -o|-J|-i|-p|-l) shift 2;; -*) shift;; *) break;; esac; done
shift
case "$*" in true) exit 0;; 'bash -s -- check-env /etc/aivoicebus/node.env') exec bash -s -- check-env "$FIXTURE_NODE_ENV";; *) : >"$FIXTURE_WRITE_MARKER"; exit 99;; esac
`
	if e := os.WriteFile(filepath.Join(bin, "ssh"), []byte(ssh), 0755); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"scp", "curl"} {
		if e := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n: >\"$FIXTURE_WRITE_MARKER\"\nexit 99\n"), 0755); e != nil {
			t.Fatal(e)
		}
	}
	cmd := exec.Command("bash", filepath.Join("..", "..", "deploy", "install.sh"), "--apply", "--tokens-only", "fixture")
	cmd.Env = append(os.Environ(), "HOME="+root, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "AIVOICEBUS_NODES_CONF="+nodes, "AIVOICEBUS_TOKEN_FILE="+token, "AIVOICEBUS_NODE_TOKENS_FILE="+token, "AIVOICEBUS_MACHINE_TOKENS_DIR="+filepath.Join(root, "empty-machines"), "AIVOICEBUS_OBSERVER_TOKEN_FILE="+filepath.Join(root, "no-observer"), "AIVOICEBUS_NODE_REQUIRE_SIGNED=", "AIVOICEBUS_NODE_LEASE_RETENTION_ARGS=", "FIXTURE_NODE_ENV="+nodeEnv, "FIXTURE_WRITE_MARKER="+marker)
	b, e := cmd.CombinedOutput()
	if e == nil {
		t.Fatal("duplicate retained config proceeded", string(b))
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatalf("staging/auth/unit write preceded retained validation: %v %s", e, b)
	}
}
