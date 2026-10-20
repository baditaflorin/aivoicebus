package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/sharestore"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	shTokA = "share-shared-token-0123456789-abcdef"
	shTokM = "share-machine-token-0123456789-abcdef"
	shTokO = "share-observer-token-0123456789-abcdef"
)

type shareNode struct {
	srv  *httptest.Server
	st   *store.Store
	sh   *sharestore.Store
	dir  string
	name string
}

// newShareNode starts a node with shared spaces over dir (kept across a restart).
func newShareNode(t *testing.T, dir, name string) *shareNode {
	t.Helper()
	st, err := store.Open(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sharestore.Open(filepath.Join(dir, "share"), name, sharestore.DefaultLimits(), ThreadActivity(st))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := ParseTokens(shTokA + "\nlap:" + shTokM + "\n")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(st, "", WithTokenSet(ts), WithObserverToken(shTokO), WithNode(name), WithShares(sh, nil)))
	n := &shareNode{srv: srv, st: st, sh: sh, dir: dir, name: name}
	t.Cleanup(n.stop)
	return n
}

func (n *shareNode) stop() {
	if n.srv != nil {
		n.srv.Close()
		n.sh.Close()
		n.srv = nil
	}
}

func (n *shareNode) client(tok string) *client.Client {
	return &client.Client{URLs: []string{n.srv.URL}, Token: tok}
}

func shSealed(n int) []byte {
	b := make([]byte, n+28)
	_, _ = rand.Read(b)
	return append([]byte(msg.ShareChunkMagic), b...)
}

func shHex(n int) string {
	b := make([]byte, n/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func TestShareRoutesHoldVersionsAndReplicate(t *testing.T) {
	a := newShareNode(t, t.TempDir(), "na")
	b := newShareNode(t, t.TempDir(), "nb")
	root, err := a.st.Send(msg.Message{From: "lap/claude.a1", To: "desk", Body: "a thread to share in"})
	if err != nil {
		t.Fatal(err)
	}
	th := root.ID
	ca := a.client(shTokA)

	// a big file in two chunks, then a second version
	big := [][]byte{shSealed(msg.ShareChunk - 28), shSealed(1000)}
	var hashes []string
	for _, c := range big {
		h, err := ca.ShareUpload(th, c)
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	name := shHex(32)
	v := msg.ShareVersion{ID: shHex(32), Name: name, KeyID: shHex(16), Chunks: hashes, Meta: "bWV0YQ==", By: "lap/claude.a1"}
	r1, err := ca.SharePut(th, v)
	if err != nil || r1.Ver.Num != 1 {
		t.Fatalf("put: %v %+v", err, r1.Ver)
	}
	v.ID = shHex(32)
	if r2, err := ca.SharePut(th, v); err != nil || r2.Ver.Num != 2 {
		t.Fatalf("second version: %v", err)
	}
	if _, err := ca.ShareKey(th, msg.ShareKey{ID: v.KeyID, For: "desk", Box: "AAAA", Sealed: "AAAA", By: "lap/claude.a1"}); err != nil {
		t.Fatal(err)
	}

	// notes refuse a stale base, with the reason and the latest version
	notes := msg.ShareVersion{ID: shHex(32), Name: shHex(32), KeyID: v.KeyID, Chunks: hashes[1:], Meta: "bWV0YQ==", By: "lap/claude.a1", Notes: true}
	if _, err := ca.SharePut(th, notes); err != nil {
		t.Fatal(err)
	}
	notes.ID = shHex(32)
	var se *client.StatusError
	if _, err := ca.SharePut(th, notes); !errors.As(err, &se) || se.Refused() != msg.RefusedConflict || !strings.Contains(se.Text, "at version 1") {
		t.Fatalf("a second first page: %v", err)
	}

	// the other node takes everything, chunks included, and a restart loses nothing
	if _, _, err := pullShares(b.sh, a.srv.URL, shTokA, io.Discard); err != nil {
		t.Fatal(err)
	}
	cb := b.client(shTokA)
	idx, err := cb.ShareIndex(th)
	if err != nil || len(idx.Records) != 4 {
		t.Fatalf("index on the other node: %v, %d records", err, len(idx.Records))
	}
	for i, h := range hashes {
		got, err := cb.ShareChunk(th, h)
		if err != nil || !bytes.Equal(got, big[i]) {
			t.Fatalf("chunk %d on the other node: %v", i, err)
		}
	}
	b.stop()
	b = newShareNode(t, b.dir, "nb")
	if idx, _ := b.client(shTokA).ShareIndex(th); len(idx.Records) != 4 {
		t.Fatalf("after a restart: %d records", len(idx.Records))
	}
	if n, _, _ := pullShares(b.sh, a.srv.URL, shTokA, io.Discard); n != 0 {
		t.Fatalf("a restarted node took %d records again", n)
	}
	h, _ := health(t, a.srv.URL)
	if !slices.Contains(h.Features, msg.FeatureShare) {
		t.Fatalf("features: %v", h.Features)
	}
}

func health(t *testing.T, url string) (msg.Health, error) {
	t.Helper()
	c := &client.Client{URLs: []string{url}, Token: shTokA}
	return c.Health()
}

func TestShareRoutesWhoMay(t *testing.T) {
	a := newShareNode(t, t.TempDir(), "na")
	root, _ := a.st.Send(msg.Message{From: "lap/claude.a1", To: "desk", Body: "who may"})
	th := root.ID
	var se *client.StatusError
	// the observer reads none of it
	if _, err := a.client(shTokO).ShareIndex(th); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("observer index: %v", err)
	}
	// a machine token writes only as its machine, and never takes the feed
	cm := a.client(shTokM)
	h, err := cm.ShareUpload(th, shSealed(10))
	if err != nil {
		t.Fatal(err)
	}
	v := msg.ShareVersion{ID: shHex(32), Name: shHex(32), KeyID: shHex(16), Chunks: []string{h}, Meta: "bWV0YQ==", By: "desk/claude.b1"}
	if _, err := cm.SharePut(th, v); !errors.As(err, &se) || se.Refused() != msg.RefusedDenied {
		t.Fatalf("a machine token writing as another machine: %v", err)
	}
	v.By = "lap/claude.a1"
	if _, err := cm.SharePut(th, v); err != nil {
		t.Fatal(err)
	}
	if _, err := cm.ShareSync(nil); !errors.As(err, &se) || se.Code != http.StatusForbidden {
		t.Fatalf("a machine token took the feed: %v", err)
	}
	// plaintext, a thread the node never saw, a chunk of another node
	if _, err := a.client(shTokA).ShareUpload(th, []byte(strings.Repeat("plain ", 20))); !errors.As(err, &se) || se.Code != http.StatusBadRequest {
		t.Fatalf("plaintext: %v", err)
	}
	if _, err := a.client(shTokA).ShareUpload("ffffffffff", shSealed(10)); !errors.As(err, &se) || se.Refused() != msg.RefusedUnknown {
		t.Fatalf("an unknown thread: %v", err)
	}
	v.ID, v.Chunks = shHex(32), []string{shHex(64)}
	if _, err := a.client(shTokA).SharePut(th, v); !errors.As(err, &se) || se.Refused() != msg.RefusedShareMissing {
		t.Fatalf("a version without its chunks: %v", err)
	}
}

// A peer from before shared spaces answers 404: nothing breaks, and the node can still take writes.
func TestSharesWithAnOldPeer(t *testing.T) {
	st, _ := store.Open(t.TempDir(), "old")
	old := httptest.NewServer(NewHandler(st, shTokA)) // no WithShares: the routes are not there
	defer old.Close()
	a := newShareNode(t, t.TempDir(), "na")
	n, isOld, err := pullShares(a.sh, old.URL, shTokA, io.Discard)
	if err != nil || !isOld || n != 0 {
		t.Fatalf("pull from an old peer: %d %v %v", n, isOld, err)
	}
	b := &shareBoard{}
	if shareReady(a.sh, b, 1)() {
		t.Fatal("ready before any peer answered, on an empty disk")
	}
	b.ok()
	if !shareReady(a.sh, b, 1)() {
		t.Fatal("not ready after the old peer answered")
	}
	if _, err := (&client.Client{URLs: []string{old.URL}, Token: shTokA}).ShareIndex("0a1b2c3d4e"); !client.IsNotFound(err) {
		t.Fatalf("an old node's index: %v", err)
	}
	h, _ := health(t, old.URL)
	if slices.Contains(h.Features, msg.FeatureShare) {
		t.Fatal("a node without shared spaces claims them")
	}
}
