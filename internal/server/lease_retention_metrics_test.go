package server

import (
	"bytes"
	"github.com/baditaflorin/aivoicebus/internal/store"
	"strings"
	"testing"
)

func TestLeaseRetentionMetricsHaveBoundedModeLabelsAndExplicitScope(t *testing.T) {
	var output bytes.Buffer
	writeLeaseRetentionMetrics(&output, store.LeaseRetentionStatus{Mode: store.LeaseRetentionOn, IndexedRecords: 123, ManagedMessages: 100, IndexBytes: 65536, ArrivalBufferSegments: 12})
	for _, want := range []string{`aivoicebus_lease_retention_mode{mode="on"} 1`, "aivoicebus_lease_managed_messages 100", "aivoicebus_lease_read_cache_bytes 0", "aivoicebus_lease_arrival_buffer_segments 12"} {
		if !strings.Contains(output.String(), want) {
			t.Fatal("missing scoped metric", want)
		}
	}
	if strings.Count(output.String(), "{") != 1 {
		t.Fatal("retention metrics gained an unbounded dimension")
	}
}
