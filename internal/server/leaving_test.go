package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/sharestore"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// A node leaving the mesh takes no client write of any kind, so nothing lands on it that its peers
// would stop pulling once it is out, and it still serves reads and its peers' feeds.
func TestALeavingNodeRefusesClientWritesAndStillFeedsItsPeers(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir, "nl")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := sharestore.Open(filepath.Join(dir, "share"), "nl", sharestore.DefaultLimits(), ThreadActivity(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sh.Close() })
	// What it holds from before it started leaving: a thread, and a version in its shared space.
	before := httptest.NewServer(NewHandler(st, shTokA, WithShares(sh, nil)))
	t.Cleanup(before.Close)
	root, err := st.Send(msg.Message{From: "lap/claude.a1", To: "desk", Body: "a thread to share in"})
	if err != nil {
		t.Fatal(err)
	}
	cb := &client.Client{URLs: []string{before.URL}, Token: shTokA}
	h, err := cb.ShareUpload(root.ID, shSealed(1000))
	if err != nil {
		t.Fatal(err)
	}
	v := msg.ShareVersion{ID: shHex(32), Name: shHex(32), KeyID: shHex(16), Chunks: []string{h}, Meta: "bWV0YQ==", By: "lap/claude.a1"}
	if _, err := cb.SharePut(root.ID, v); err != nil {
		t.Fatal(err)
	}

	leaving := httptest.NewServer(NewHandler(st, shTokA, WithShares(sh, nil), WithNode("nl"), WithLeaving(true)))
	t.Cleanup(leaving.Close)
	call := func(method, path, body string) (int, string, string) {
		t.Helper()
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, leaving.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+shTokA)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get(msg.HeaderRefused), string(b)
	}
	put, _ := json.Marshal(msg.SharePut{Thread: root.ID, Ver: msg.ShareVersion{ID: shHex(32), Name: shHex(32), KeyID: v.KeyID, Chunks: []string{h}, Meta: "bWV0YQ==", By: "lap/claude.a1"}})
	key, _ := json.Marshal(msg.ShareKeyPut{Thread: root.ID, Key: msg.ShareKey{ID: v.KeyID, For: "desk", Box: "AAAA", Sealed: "AAAA", By: "lap/claude.a1"}})
	chunk, _ := json.Marshal(msg.ShareChunkPut{Data: shSealed(10)})
	for _, w := range []struct{ path, body string }{
		{"/v1/send", `{"from":"lap/claude.a1","to":"desk","body":"hello"}`},
		{"/v1/ack", `{"id":"` + root.ID + `","by":"desk/claude.d1"}`},
		{"/v1/retract", `{"id":"` + root.ID + `","by":"lap/claude.a1"}`},
		{"/v1/files", string(sealedShape(10))},
		{"/v1/share/upload?thread=" + root.ID, string(chunk)},
		{"/v1/share/put", string(put)},
		{"/v1/share/key", string(key)},
		{"/v1/decide", `{"id":"` + root.ID + `","code":"x","say":"approve"}`},
	} {
		code, reason, text := call(http.MethodPost, w.path, w.body)
		if code != http.StatusServiceUnavailable || reason != msg.RefusedLeaving || !strings.Contains(text, "use another node") {
			t.Errorf("POST %s: %d %q %q, want 503 %q", w.path, code, reason, text, msg.RefusedLeaving)
		}
	}
	if st.Stats().Messages != 1 {
		t.Errorf("nothing was written: %+v", st.Stats())
	}

	// reads, and what its peers pull
	if code, _, body := call(http.MethodGet, "/v1/sync", ""); code != 200 || !strings.Contains(body, root.ID) {
		t.Errorf("the message feed: %d %.200s", code, body)
	}
	pst, _ := store.Open(t.TempDir(), "peer")
	other, err := sharestore.Open(t.TempDir(), "peer", sharestore.DefaultLimits(), ThreadActivity(pst))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	if n, _, err := pullShares(other, leaving.URL, shTokA, io.Discard); err != nil || n == 0 {
		t.Errorf("a peer pulls the shared space from it: %d %v", n, err)
	}
	if code, _, _ := call(http.MethodGet, "/v1/inbox?me=desk/claude.d1&peek=1", ""); code != 200 {
		t.Errorf("an inbox read: %d", code)
	}
	cl := &client.Client{URLs: []string{leaving.URL}, Token: shTokA}
	if idx, err := cl.ShareIndex(root.ID); err != nil || len(idx.Records) == 0 {
		t.Errorf("a shared space read: %v %+v", err, idx)
	}
	hl, err := cl.Health()
	if err != nil || !hl.Leaving {
		t.Errorf("health says it is leaving: %v %+v", err, hl)
	}
	if hb, _ := (&client.Client{URLs: []string{before.URL}, Token: shTokA}).Health(); hb.Leaving {
		t.Error("a node that is not leaving says nothing")
	}
}

// A client whose first node is leaving writes past it: a shared space's chunks and version land
// together on the next node, and a message reaches the others.
func TestAClientWritesPastALeavingNode(t *testing.T) {
	a := newShareNode(t, t.TempDir(), "na")
	dir := t.TempDir()
	st, _ := store.Open(dir, "nl")
	sh, err := sharestore.Open(filepath.Join(dir, "share"), "nl", sharestore.DefaultLimits(), ThreadActivity(st))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sh.Close() })
	leaving := httptest.NewServer(NewHandler(st, shTokA, WithShares(sh, nil), WithLeaving(true)))
	t.Cleanup(leaving.Close)

	c := &client.Client{URLs: []string{leaving.URL, a.srv.URL}, Token: shTokA, NoSign: true}
	root, err := c.Send(msg.Message{From: "lap/claude.a1", To: "desk", Body: "a thread to share in"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.st.Thread(root.ID)) != 1 || len(st.Thread(root.ID)) != 0 {
		t.Fatalf("the message is on the node that takes writes only: %d %d", len(a.st.Thread(root.ID)), len(st.Thread(root.ID)))
	}
	h, err := c.ShareUpload(root.ID, shSealed(1000))
	if err != nil {
		t.Fatal(err)
	}
	v := msg.ShareVersion{ID: shHex(32), Name: shHex(32), KeyID: shHex(16), Chunks: []string{h}, Meta: "bWV0YQ==", By: "lap/claude.a1"}
	if r, err := c.SharePut(root.ID, v); err != nil || r.Ver.Num != 1 {
		t.Fatalf("put: %v %+v", err, r)
	}
	if idx, err := a.client(shTokA).ShareIndex(root.ID); err != nil || len(idx.Records) == 0 {
		t.Errorf("the version is on the node that takes writes: %v %+v", err, idx)
	}
	if len(sh.VV()) != 0 {
		t.Errorf("and nothing is on the leaving one: %v", sh.VV())
	}
}
