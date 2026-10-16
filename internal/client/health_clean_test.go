package client

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// What a node says about itself, and its 4xx text, reach callers only as safe single lines.
func TestANodesOwnWordsComeBackAsSafeLines(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"node":"n\u001b[2J\u0085x","version":"1‮.0","features":["a\u0085b"],"peers":[{"url":"http://p\u0085","error":"boom\nline2\u009b"}]}`))
			return
		}
		http.Error(w, "bad\u001b[31m thing\u0085\nsecond", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &Client{URLs: []string{srv.URL}, Token: "t"}
	h, err := c.Health()
	if err != nil {
		t.Fatal(err)
	}
	all := h.Node + h.Version + h.Features[0] + h.Peers[0].URL + h.Peers[0].Error
	for _, bad := range []string{"\x1b", "\u0085", "\u009b", "‮", "\n"} {
		if strings.Contains(all, bad) {
			t.Errorf("%q survived in %q", bad, all)
		}
	}
	_, err = c.Inbox("a/b", false)
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\u0085\n") {
		t.Errorf("%q", err)
	}
}
