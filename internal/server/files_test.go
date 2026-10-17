package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/store"
)

// sealedShape is bytes with the envelope's shape (version 1, IV, MAC, ciphertext); the node
// cannot tell it from a real one, which is the point: it never holds a key.
func sealedShape(n int) []byte {
	raw := make([]byte, 49+n)
	raw[0] = 1
	for i := 1; i < len(raw); i++ {
		raw[i] = byte(i)
	}
	return []byte(base64.StdEncoding.EncodeToString(raw))
}

func fileNode(t *testing.T) *httptest.Server {
	t.Helper()
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok", WithObserverToken("obs")))
	t.Cleanup(srv.Close)
	return srv
}

func fileCall(t *testing.T, method, url, token string, body []byte) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = strings.NewReader(string(body))
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func upload(t *testing.T, base, q string, body []byte) string {
	t.Helper()
	code, out := fileCall(t, "POST", base+"/v1/files"+q, "tok", body)
	if code != http.StatusCreated {
		t.Fatalf("upload: %d %s", code, out)
	}
	var r struct{ ID string }
	if json.Unmarshal([]byte(out), &r) != nil || !reFileID.MatchString(r.ID) {
		t.Fatalf("upload answer %q", out)
	}
	return r.ID
}

func TestNodeHoldsAFileUntilItIsRead(t *testing.T) {
	srv := fileNode(t)
	env := sealedShape(100)
	id := upload(t, srv.URL, "?burn=1", env)
	if code, out := fileCall(t, "GET", srv.URL+"/v1/files/"+id, "tok", nil); code != 200 || out != string(env) {
		t.Fatalf("first read: %d %q", code, out)
	}
	if code, _ := fileCall(t, "GET", srv.URL+"/v1/files/"+id, "tok", nil); code != http.StatusNotFound {
		t.Errorf("second read of a burn file: %d, want 404", code)
	}
	// The sender can still learn it was read.
	if code, out := fileCall(t, "GET", srv.URL+"/v1/files/"+id+"/read", "tok", nil); code != 200 || !strings.Contains(out, `"read":true`) {
		t.Errorf("watch after the read: %d %s", code, out)
	}

	kept := upload(t, srv.URL, "", env)
	for i := 0; i < 2; i++ {
		if code, _ := fileCall(t, "GET", srv.URL+"/v1/files/"+kept, "tok", nil); code != 200 {
			t.Errorf("read %d of a kept file: %d", i, code)
		}
	}
}

func TestNodeFilesRefuseObserversStrangersAndPlaintext(t *testing.T) {
	srv := fileNode(t)
	env := sealedShape(10)
	if code, _ := fileCall(t, "POST", srv.URL+"/v1/files", "obs", env); code != http.StatusForbidden {
		t.Errorf("observer upload: %d, want 403", code)
	}
	if code, _ := fileCall(t, "POST", srv.URL+"/v1/files", "nope", env); code != http.StatusUnauthorized {
		t.Errorf("no token: %d, want 401", code)
	}
	id := upload(t, srv.URL, "?burn=1", env)
	if code, _ := fileCall(t, "GET", srv.URL+"/v1/files/"+id, "obs", nil); code != http.StatusForbidden {
		t.Errorf("observer read (would burn it): %d, want 403", code)
	}
	if code, _ := fileCall(t, "GET", srv.URL+"/v1/files/"+id, "tok", nil); code != 200 {
		t.Errorf("the observer's try burned the file: %d", code)
	}
	for _, body := range []string{"a plaintext report, sent by mistake\n", base64.StdEncoding.EncodeToString(append([]byte{2}, make([]byte, 60)...)), ""} {
		if code, _ := fileCall(t, "POST", srv.URL+"/v1/files", "tok", []byte(body)); code != http.StatusBadRequest {
			t.Errorf("non-envelope %.20q: %d, want 400", body, code)
		}
	}
	if code, _ := fileCall(t, "POST", srv.URL+"/v1/files", "tok", sealedShape(MaxFileEnvelope)); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized: %d, want 413", code)
	}
	if code, _ := fileCall(t, "GET", srv.URL+"/v1/files", "tok", nil); code != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/files: %d", code)
	}
	for _, p := range []string{"/v1/files/short", "/v1/files/" + id + "/x", "/v1/files/" + strings.Repeat("A", 22)} {
		if code, _ := fileCall(t, "GET", srv.URL+p, "tok", nil); code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
	// The usual cap still holds everywhere else.
	big := `{"from":"a/b","to":"c/d","body":"` + strings.Repeat("x", 1<<20) + `"}`
	if code, _ := fileCall(t, "POST", srv.URL+"/v1/send", "tok", []byte(big)); code == http.StatusOK || code == http.StatusCreated {
		t.Errorf("a 1 MB send got through: %d", code)
	}
}

func TestFileBoxExpiresCapsAndWatches(t *testing.T) {
	b := newFileBox()
	now := time.Now()
	b.now = func() time.Time { return now }
	id, _, err := b.put([]byte("x"), time.Minute, true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := b.get(id); ok {
		t.Error("an expired file was served")
	}
	if b.bytes != 0 || len(b.files) != 0 {
		t.Errorf("expired file still counted: %d bytes, %d files", b.bytes, len(b.files))
	}

	chunk := make([]byte, maxFilesBytes/4)
	for i := 0; i < 4; i++ {
		if _, _, err := b.put(chunk, time.Minute, true); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
	if _, _, err := b.put([]byte("one more"), time.Minute, true); err == nil {
		t.Error("the box took more than maxFilesBytes")
	}
	now = now.Add(2 * time.Minute) // all expire: room again
	if _, _, err := b.put([]byte("y"), time.Minute, true); err != nil {
		t.Errorf("after expiry: %v", err)
	}

	b.now = time.Now
	w, _, _ := b.put([]byte("z"), time.Minute, true)
	if read, found := b.watch(context.Background(), w, 10*time.Millisecond); read || !found {
		t.Errorf("watch before the read: read=%v found=%v", read, found)
	}
	go func() { time.Sleep(20 * time.Millisecond); b.get(w) }()
	if read, _ := b.watch(context.Background(), w, 5*time.Second); !read {
		t.Error("watch missed the read")
	}
	if _, found := b.watch(context.Background(), "nothere", time.Millisecond); found {
		t.Error("watch found an id never stored")
	}
}
