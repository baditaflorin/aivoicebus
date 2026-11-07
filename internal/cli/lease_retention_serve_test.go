package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestServeLeaseRetentionOptionsDefaultOffAndExplicitAllowlist(t *testing.T) {
	o, err := leaseRetentionOptions("off", "", 30*24*time.Hour)
	if err != nil || o.Mode != store.LeaseRetentionOff || len(o.Hosts) != 0 {
		t.Fatal(o, err)
	}
	o, err = leaseRetentionOptions("on", "0docker-builder, builder-two", 30*24*time.Hour)
	if err != nil || len(o.Hosts) != 2 || o.Hosts[1] != "builder-two" {
		t.Fatal(o, err)
	}
	for _, test := range []struct {
		mode, hosts string
		keep        time.Duration
	}{{"bad", "", 720 * time.Hour}, {"on", "", 720 * time.Hour}, {"on", "*", 720 * time.Hour}, {"on", "builder/a", 720 * time.Hour}, {"on", "builder,", 720 * time.Hour}, {"on", "bad host", 720 * time.Hour}, {"on", "builder", time.Hour}} {
		if _, err := leaseRetentionOptions(test.mode, test.hosts, test.keep); err == nil {
			t.Fatal("unsafe retention options accepted", test)
		}
	}
}
func TestServeInvalidLeaseRetentionRefusesBeforeCredentialsOrDataCreation(t *testing.T) {
	for _, args := range [][]string{{"--lease-retention=invalid"}, {"--lease-retention=on"}, {"--lease-retention=on", "--lease-retention-hosts=*"}, {"--lease-retention-keep=1h"}} {
		data := filepath.Join(t.TempDir(), "not-created")
		all := append([]string{"--data", data, "--token-file", filepath.Join(data, "missing-token")}, args...)
		err := serve(all, Streams{Out: io.Discard, Err: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "lease retention:") {
			t.Fatal("retention error did not precede token/data access", args, err)
		}
		if _, err := os.Lstat(data); !os.IsNotExist(err) {
			t.Fatal("invalid config created data files", err)
		}
	}
}
