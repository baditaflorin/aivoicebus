package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestLeaseHistorySyncReadFailureReturnsUnavailableNotEmptySuccess(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2031, 6, 1, 12, 0, 0, 0, time.UTC)
	exp := now.Add(-45 * 24 * time.Hour)
	from := "0docker-builder/fleet-runner." + strings.Repeat("1", 32)
	record := msg.Record{Origin: "fixture-peer", Seq: 1, Msg: &msg.Message{
		ID: "0000000001", From: from, To: from, Thread: "0000000001", Kind: msg.KindLease,
		Body: msg.LeaseBody("deploy:prod:go_example", "synthetic history"), At: exp.Add(-time.Hour), Exp: &exp,
	}}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "log.jsonl")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(dir, "fixture-local", store.WithClock(func() time.Time { return now }),
		store.WithLeaseRetention(store.LeaseRetentionOptions{Mode: store.LeaseRetentionObserve,
			Keep: 30 * 24 * time.Hour, Hosts: []string{"0docker-builder"}}))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	token := "fixture-" + "only"
	handler := NewHandler(s, token)
	request := func() *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/v1/sync?vv=%7B%7D", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	before := request()
	var got []msg.Record
	if before.Code != http.StatusOK || json.Unmarshal(before.Body.Bytes(), &got) != nil || len(got) != 1 || got[0].Msg.ID != record.Msg.ID {
		t.Fatalf("healthy control did not serve the original record: status%d body%s", before.Code, before.Body.String())
	}
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	after := request()
	if after.Code != http.StatusServiceUnavailable {
		t.Fatalf("indexed read failure was served as status%d body%s", after.Code, after.Body.String())
	}
	if strings.TrimSpace(after.Body.String()) == "[]" || strings.Contains(after.Body.String(), record.Msg.Body) {
		t.Fatal("failure masqueraded as an empty feed or disclosed record contents")
	}
	if vv := s.VV(); vv[record.Origin] != 1 {
		t.Fatalf("read failure rewrote authoritative version vector: %v", vv)
	}
}
