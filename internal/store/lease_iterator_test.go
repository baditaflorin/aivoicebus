package store

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func collectLeaseIterator(t *testing.T, it *LeaseIterator) []msg.Lease {
	t.Helper()
	defer it.Close()
	out := []msg.Lease{}
	for {
		page, err := it.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(page) > 256 {
			t.Fatal("unbounded payload page")
		}
		out = append(out, page...)
	}
}

func TestLeaseIteratorCompleteBeyondCacheBudgetAndMatchesLegacyOrder(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	const count = 5003
	var records []msg.Record
	for i := 0; i < count; i++ {
		m := projectionLease(i, now, now.Add(time.Duration(count-i)*time.Second))
		records = append(records, msg.Record{Origin: "peer", Seq: uint64(len(records) + 1), Msg: &m})
		if i == 1 {
			operator := m
			operator.ID = "a000000001"
			operator.Thread = operator.ID
			operator.From = "operator/session"
			operator.To = operator.From
			operator.Body = "deploy:prod\noperator freeze"
			records = append(records, msg.Record{Origin: "peer", Seq: uint64(len(records) + 1), Msg: &operator})
		}
	}
	withdraw := msg.Ack{ID: msg.RetractPrefix + fmt.Sprintf("%010x", 11), By: "0docker-builder/operator", At: now}
	records = append(records, msg.Record{Origin: "peer", Seq: uint64(len(records) + 1), Ack: &withdraw})
	s, _ := projectionFixture(t, records, now)
	want := s.Leases()
	got, err := s.LeasesChecked()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count || !reflect.DeepEqual(got, want) {
		t.Fatalf("complete legacy order/holds lost: got%d want%d", len(got), len(want))
	}
	// The fixture deliberately sets hot/cache budgets to just two records/64B.
	// These cap no logical population and do not reject valid selected claims.
	it, err := s.NewLeaseIterator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := collectLeaseIterator(t, it); !reflect.DeepEqual(got, want) {
		t.Fatal("paged path differs from complete checked query")
	}
}

func TestLeaseIteratorKeepsOldWithdrawalAndRawEndSnapshot(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	m := projectionLease(0, now, now.Add(time.Hour))
	s, _ := projectionFixture(t, []msg.Record{{Origin: "peer", Seq: 1, Msg: &m}}, now)
	it, err := s.NewLeaseIterator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(msg.RetractPrefix+m.ID, m.From); err != nil {
		t.Fatal(err)
	}
	newer := projectionLease(1, now, now.Add(2*time.Hour))
	if _, err := s.Send(newer); err != nil {
		t.Fatal(err)
	}
	old := collectLeaseIterator(t, it)
	if len(old) != 1 || old[0].ID != m.ID {
		t.Fatal("a later withdrawal or claim changed a paused snapshot", old)
	}
	current, err := s.LeasesChecked()
	if err != nil || len(current) != 1 || current[0].ID != newer.ID {
		t.Fatal("new query did not observe the new state", err)
	}
}

func TestLeaseIteratorResumesAfterRewriteAndRejectsMidstreamIO(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	exp := now.Add(time.Minute)
	tid := msg.NewTestID()
	lane := msg.Message{ID: tid, Thread: tid, From: "fixture/a", To: "fixture/b", At: now, Exp: &exp, Body: strings.Repeat("fixture ", 60)}
	records := []msg.Record{{Origin: "peer", Seq: 1, Msg: &lane}}
	for i := 0; i < 600; i++ {
		m := projectionLease(i, now, now.Add(time.Hour))
		records = append(records, msg.Record{Origin: "peer", Seq: uint64(len(records) + 1), Msg: &m})
	}
	s, _ := projectionFixture(t, records, now)
	it, err := s.NewLeaseIterator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.SetClock(func() time.Time { return now.Add(6 * time.Minute) })
	done := make(chan struct{})
	go func() { s.Stats(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("paused iterator blocked test-lane rewrite")
	}
	if got := collectLeaseIterator(t, it); len(got) != 600 {
		t.Fatal("rewrite lost the pinned generation's holds")
	}
	broken, err := s.NewLeaseIterator(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	if page, err := broken.Next(); err != nil || len(page) != 256 {
		t.Fatal("healthy first page failed", err)
	}
	if err := os.Truncate(filepath.Join(s.Dir(), "log.jsonl"), 0); err != nil {
		t.Fatal(err)
	}
	if page, err := broken.Next(); err == nil || err == io.EOF || page != nil {
		t.Fatal("midstream failure returned a successful or partial page", err)
	}
	if s.HealthChecked() == nil {
		t.Fatal("real current-generation I/O failure was not latched unhealthy")
	}
}

func TestLeaseIteratorSerializesClockRollbackAndExpiryPreparation(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	var records []msg.Record
	for i := 0; i < 20; i++ {
		m := projectionLease(i, now, now.Add(time.Minute))
		records = append(records, msg.Record{Origin: "peer", Seq: uint64(i + 1), Msg: &m})
	}
	s, _ := projectionFixture(t, records, now)
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for worker := 0; worker < 2; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				selected := now
				if (i+worker)%2 == 0 {
					selected = now.Add(2 * time.Minute)
				}
				s.SetClock(func() time.Time { return selected })
				it, err := s.NewLeaseIterator(context.Background())
				if err != nil {
					errs <- err
					return
				}
				count := 0
				at := it.at
				for {
					page, err := it.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						it.Close()
						errs <- err
						return
					}
					count += len(page)
				}
				it.Close()
				want := 20
				if !at.Before(now.Add(time.Minute)) {
					want = 0
				}
				if count != want {
					errs <- fmt.Errorf("snapshot at%s lost holds: got%d want%d", at, count, want)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
