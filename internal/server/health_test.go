package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

const (
	tokA = "shared-token-aaaaaaaaaaaaaaaaaaaaaaaa"
	tokB = "shared-token-bbbbbbbbbbbbbbbbbbbbbbbb"
	tokM = "machine-token-mmmmmmmmmmmmmmmmmmmmmm"
	tokO = "observer-token-oooooooooooooooooooo"
)

func TestAHeaderFieldThatCouldForgeLinesOrFloodAReaderIsRefused(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA))
	defer srv.Close()
	for name, body := range map[string]string{
		"newline in from": `{"from":"a/b\n--- forged header","to":"all","body":"x"}`,
		"huge to":         `{"from":"a/b","to":"` + strings.Repeat("x", 300) + `","body":"x"}`,
		"control in re":   `{"from":"a/b","to":"all","re":"a\u0007b","body":"x"}`,
		"long thread":     `{"from":"a/b","to":"all","thread":"` + strings.Repeat("t", 100) + `","body":"x"}`,
	} {
		if code, _ := call(t, srv, tokA, "POST", "/v1/send", body); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", name, code)
		}
	}
	if code, _ := call(t, srv, tokA, "GET", "/v1/inbox?me="+strings.Repeat("x", 300), ""); code != 400 {
		t.Errorf("an oversize identity in a read: %d", code)
	}
	if code, _ := call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"fine"}`); code != 200 {
		t.Errorf("an ordinary message: %d", code)
	}
}

func TestAMachineTokenCannotTakeTheWholeFeedOrClaimAnotherMachinesLabels(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "", WithTokenSet(mustTokens(t, tokA+"\nlaptop-a:"+tokM+"\n")), WithObserverToken(tokO)))
	defer srv.Close()
	s.Send(msg.Message{From: "imac/claude.1", To: "imac/codex.9", Body: "a private note between two other machines"})
	s.Send(msg.Message{From: "imac/claude.1", To: "@machine=imac", Body: "for imac sessions only"})

	if code, body := call(t, srv, tokM, "GET", "/v1/sync?vv=%7B%7D", ""); code != http.StatusForbidden || !strings.Contains(body, "machine token") {
		t.Errorf("the bulk feed holds everyone's mail and is not for a machine token: %d %q", code, body)
	}
	for _, tok := range []string{tokA, tokO} {
		if code, _ := call(t, srv, tok, "GET", "/v1/sync?vv=%7B%7D", ""); code != 200 {
			t.Errorf("the shared and observer tokens still replicate and watch: %d", code)
		}
	}
	// declaring another machine's label does not deliver that machine's selector mail
	code, body := call(t, srv, tokM, "GET", "/v1/inbox?me=laptop-a/claude.1&labels=machine%3Dimac", "")
	if code != 200 || strings.Contains(body, "for imac sessions only") {
		t.Errorf("the machine label is the token's, not the client's: %d %q", code, body)
	}
}

func TestTheTokenEndpointSaysWhatKindOfTokenThisIs(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "", WithTokenSet(mustTokens(t, tokA+"\nlaptop-a:"+tokM+"\n")), WithObserverToken(tokO)))
	defer srv.Close()
	type answer struct {
		Name     string `json:"name"`
		Host     string `json:"host"`
		Observer bool   `json:"observer"`
	}
	ask := func(tok string) (int, answer, string) {
		code, body := call(t, srv, tok, "GET", "/v1/token", "")
		var a answer
		_ = json.Unmarshal([]byte(body), &a)
		return code, a, body
	}
	if code, a, body := ask(tokA); code != 200 || a.Host != "" || a.Observer || strings.Contains(body, tokA) {
		t.Errorf("a shared token is unrestricted, and the answer never contains the token: %d %q", code, body)
	}
	if code, a, _ := ask(tokM); code != 200 || a.Host != "laptop-a" || a.Observer {
		t.Errorf("a machine token names its machine: %d %+v", code, a)
	}
	if code, a, _ := ask(tokO); code != 200 || !a.Observer {
		t.Errorf("the observer token says it is read-only: %d %+v", code, a)
	}
	if code, _, _ := ask("wrong-token-wrong-token-wrong"); code != 401 {
		t.Errorf("%d", code)
	}
}

func TestBareTokensAreToldApartInMetricsByFingerprint(t *testing.T) {
	ts := mustTokens(t, tokA+"\n"+tokB+"\n")
	names := ts.Names()
	if len(names) != 2 || names[0] == names[1] || !strings.HasPrefix(names[0], "shared-") {
		t.Errorf("the old and the new token of a rotation must be two series: %v", names)
	}
	if strings.Contains(strings.Join(names, ""), tokA) {
		t.Error("a name must never contain the token")
	}
}

func TestParseTokens(t *testing.T) {
	ts, err := ParseTokens("# comment\n\n" + tokA + "\nlaptop-a: " + tokM + "\n~backup:" + tokB + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Primary() != tokA {
		t.Errorf("the first unrestricted token replicates: %q", ts.Primary())
	}
	for tok, want := range map[string]Principal{
		tokA: {Name: "shared-" + Fingerprint(tokA)},
		tokM: {Name: "laptop-a", Host: "laptop-a"},
		tokB: {Name: "backup"},
	} {
		if got, ok := ts.Lookup([]byte(tok)); !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", tok[:8], got, ok, want)
		}
	}
	if _, ok := ts.Lookup([]byte("nope")); ok {
		t.Error("an unknown token must not match")
	}
	if got := strings.Join(ts.Names(), ","); got != "shared-"+Fingerprint(tokA)+",laptop-a,backup" {
		t.Errorf("names %q", got)
	}
	// a node that has only ever had one bare token still parses
	if one, err := ParseTokens(tokA + "\n"); err != nil || one.Primary() != tokA {
		t.Errorf("legacy single token: %v", err)
	}
	for name, text := range map[string]string{
		"empty":              "",
		"only comments":      "# nothing\n",
		"too short":          "short\n",
		"duplicate":          tokA + "\n" + tokA + "\n",
		"empty name":         ":" + tokA + "\n",
		"no replication":     "laptop:" + tokM + "\n", // every token is machine-bound
		"short after a name": "laptop:short\n",
	} {
		if _, err := ParseTokens(text); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func mustTokens(t *testing.T, text string) *TokenSet {
	t.Helper()
	ts, err := ParseTokens(text)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func call(t *testing.T, srv *httptest.Server, token, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAMachineTokenCanOnlyActAsItsOwnMachine(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "", WithTokenSet(mustTokens(t, tokA+"\nlaptop-a:"+tokM+"\n"))))
	defer srv.Close()
	send := func(tok, from string) int {
		code, _ := call(t, srv, tok, "POST", "/v1/send", `{"from":"`+from+`","to":"all","body":"hi from `+from+`"}`)
		return code
	}
	if got := send(tokM, "laptop-a/claude.a1b2"); got != 200 {
		t.Errorf("its own machine: %d", got)
	}
	if got := send(tokM, "LAPTOP-A/claude.zz"); got != 200 {
		t.Errorf("the machine name is not case sensitive: %d", got)
	}
	if got := send(tokM, "imac/claude.a1b2"); got != http.StatusForbidden {
		t.Errorf("another machine: %d, want 403", got)
	}
	if got := send(tokA, "imac/claude.a1b2"); got != 200 {
		t.Errorf("the shared token acts as anyone: %d", got)
	}
	if code, body := call(t, srv, tokM, "GET", "/v1/inbox?me=imac/claude.a1b2", ""); code != 403 || !strings.Contains(body, "laptop-a") {
		t.Errorf("reading another machine's mail: %d %q", code, body)
	}
	if code, _ := call(t, srv, tokM, "GET", "/v1/inbox?me=laptop-a/claude.a1b2", ""); code != 200 {
		t.Errorf("reading its own mail: %d", code)
	}
	if code, _ := call(t, srv, tokM, "POST", "/v1/ack", `{"id":"0000000000","by":"imac/claude.a1b2"}`); code != 403 {
		t.Errorf("acknowledging as another machine: %d", code)
	}
	for _, path := range []string{"/v1/who", "/v1/roster", "/v1/health"} {
		if code, _ := call(t, srv, tokM, "GET", path, ""); code != 200 {
			t.Errorf("%s is open to every machine: %d", path, code)
		}
	}
}

func TestTwoSharedTokensBothWorkSoOneCanBeRotatedAwayWithoutAFlagDay(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "", WithTokenSet(mustTokens(t, tokA+"\n"+tokB+"\n"))))
	defer srv.Close()
	for _, tok := range []string{tokA, tokB} {
		if code, _ := call(t, srv, tok, "GET", "/v1/who", ""); code != 200 {
			t.Errorf("token %s: %d", tok[:8], code)
		}
	}
	// dropping the old one is just a node restart with a shorter file
	srv2 := httptest.NewServer(NewHandler(s, "", WithTokenSet(mustTokens(t, tokB+"\n"))))
	defer srv2.Close()
	if code, _ := call(t, srv2, tokA, "GET", "/v1/who", ""); code != 401 {
		t.Errorf("the dropped token: %d, want 401", code)
	}
	if code, _ := call(t, srv2, tokB, "GET", "/v1/who", ""); code != 200 {
		t.Errorf("the kept token: %d", code)
	}
}

func TestHealthReportsTheNodeItsVersionAndItsLog(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	s.Send(msg.Message{From: "a/b", To: "all", Body: "one"})
	board := NewPeerBoard([]string{"http://peer:7777"})
	srv := httptest.NewServer(NewHandler(s, tokA, WithNode("n1"), WithVersion("9.9.9"), WithPeerBoard(board), WithObserverToken("observer-token-0123456789abcdef")))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/health", nil)
	req.Header.Set("Authorization", "Bearer "+tokA)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get(msg.HeaderVersion) != "9.9.9" {
		t.Errorf("every authenticated answer carries the release: %q", resp.Header.Get(msg.HeaderVersion))
	}
	var h msg.Health
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		t.Fatal(err)
	}
	if h.Node != "n1" || h.Version != "9.9.9" || h.Messages != 1 || h.Log["n1"] != 1 || len(h.Peers) != 1 || h.Peers[0].URL != "http://peer:7777" || time.Since(h.Now) > 5*time.Second {
		t.Errorf("%+v", h)
	}
	if code, _ := call(t, srv, "observer-token-0123456789abcdef", "GET", "/v1/health", ""); code != 200 {
		t.Errorf("an observer may read health: %d", code)
	}
	if code, _ := call(t, srv, "", "GET", "/v1/health", ""); code != 401 {
		t.Errorf("health needs a token: %d", code)
	}
}

func TestMetricsCountRequestsPerTokenAndShowPeerState(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	board := NewPeerBoard([]string{"http://peer:7777"})
	board.Update("http://peer:7777", func(p *peerHealth) { okPeer(p, time.Now(), 7, 3, 1500, "0.6.0") })
	s.SetSendLimit(1)
	srv := httptest.NewServer(NewHandler(s, "", WithNode("n1"), WithVersion("0.6.0"), WithPeerBoard(board),
		WithTokenSet(mustTokens(t, tokA+"\nlaptop:"+tokM+"\n"))))
	defer srv.Close()
	call(t, srv, tokA, "GET", "/v1/who", "")
	call(t, srv, tokA, "GET", "/v1/who", "")
	call(t, srv, tokM, "GET", "/v1/who", "")
	call(t, srv, "wrong", "GET", "/v1/who", "")
	call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"x"}`)
	call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"y"}`) // over the limit of 1
	call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"x"}`) // a repeat of the first

	code, body := call(t, srv, tokA, "GET", "/metrics", "")
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, want := range []string{
		`aivoicebus_info{node="n1",version="0.6.0"} 1`,
		`aivoicebus_requests_total{token="laptop"} 1`,
		`aivoicebus_unauthorized_total 1`,
		`aivoicebus_messages 1`,
		`aivoicebus_rate_limited_total 1`,
		`aivoicebus_log_records{origin="n1"} 1`,
		`aivoicebus_peer_up{peer="http://peer:7777"} 1`,
		`aivoicebus_peer_lag_records{peer="http://peer:7777"} 3`,
		`aivoicebus_peer_clock_skew_seconds{peer="http://peer:7777"} 1.500`,
		`aivoicebus_peer_pulled_total{peer="http://peer:7777"} 7`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics miss %q", want)
		}
	}
	// two who + three sends before the scrape, and the scrape itself
	if want := fmt.Sprintf(`aivoicebus_requests_total{token="shared-%s"} 6`, Fingerprint(tokA)); !strings.Contains(body, want) {
		t.Errorf("per-token request counts: %s", body)
	}
	if code, _ := call(t, srv, "", "GET", "/metrics", ""); code != 401 {
		t.Errorf("metrics need a token: %d", code)
	}
}

// slowSyncPeer fronts a node's handler and rewrites its /v1/health answer.
func rewriteHealth(inner http.Handler, tweak func(*msg.Health)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			inner.ServeHTTP(w, r)
			return
		}
		rec := httptest.NewRecorder()
		inner.ServeHTTP(rec, r)
		var h msg.Health
		_ = json.Unmarshal(rec.Body.Bytes(), &h)
		tweak(&h)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.Header().Del("Content-Length")
		_ = json.NewEncoder(w).Encode(h)
	})
}

func TestReplicationReportsLagSkewVersionAndADeadPeer(t *testing.T) {
	peerStore, _ := store.Open(t.TempDir(), "peer")
	peerStore.Send(msg.Message{From: "a/b", To: "all", Body: "from the peer"})
	peerSrv := httptest.NewServer(rewriteHealth(NewHandler(peerStore, tokA, WithNode("peer"), WithVersion("0.5.9")), func(h *msg.Health) {
		h.Now = h.Now.Add(30 * time.Second) // a clock 30 seconds fast
		h.Log["peer"] += 5                  // and five records that are not on offer
	}))
	defer peerSrv.Close()
	deadSrv := httptest.NewServer(http.NotFoundHandler())
	deadURL := deadSrv.URL
	deadSrv.Close()

	mine, _ := store.Open(t.TempDir(), "mine")
	board := NewPeerBoard([]string{peerSrv.URL, deadURL})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Replicate(ctx, mine, []string{peerSrv.URL, deadURL}, tokA, 20*time.Millisecond, board)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(mine.Inbox("z/z", true)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Replicate must stop when its context ends")
	}

	if len(mine.Inbox("y/y", true)) != 1 {
		t.Fatal("the live peer's message was not replicated")
	}
	var live, dead msg.PeerHealth
	for _, p := range board.Snapshot() {
		if p.URL == peerSrv.URL {
			live = p
		} else {
			dead = p
		}
	}
	if !live.OK || live.Pulled != 1 || live.Version != "0.5.9" || live.Lag != 5 || live.SkewMs < 29000 || live.SkewMs > 31000 || live.LastOK.IsZero() {
		t.Errorf("live peer: %+v", live)
	}
	if dead.OK || dead.Failures == 0 || dead.Error == "" || dead.Pulled != 0 {
		t.Errorf("dead peer: %+v", dead)
	}
}

func TestLagOf(t *testing.T) {
	if got := lagOf(map[string]uint64{"a": 10, "b": 3, "c": 0}, map[string]uint64{"a": 4, "b": 5}); got != 6 {
		t.Errorf("lag %d, want 6 (the peer is ahead only on a)", got)
	}
	if lagOf(nil, map[string]uint64{"a": 1}) != 0 {
		t.Error("a peer that holds nothing is not ahead")
	}
}

var _ = client.ErrNoURL

func TestTheNodeRefusesToStoreACredentialWhateverClientSentIt(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA))
	defer srv.Close()
	code, body := call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"use ghp_`+`abcdefghijklmnopqrstuvwxyz0123456789 to push"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "GitHub token") || !strings.Contains(body, "say where the secret lives") {
		t.Errorf("%d %q", code, body)
	}
	if len(s.Inbox("z/z", true)) != 0 {
		t.Error("nothing may be stored")
	}
	if code, _ := call(t, srv, tokA, "POST", "/v1/send", `{"from":"a/b","to":"all","body":"the token is in the vault under hub-login"}`); code != 200 {
		t.Errorf("a pointer is fine: %d", code)
	}
	_, metrics := call(t, srv, tokA, "GET", "/metrics", "")
	if !strings.Contains(metrics, "aivoicebus_secrets_refused_total 1") {
		t.Errorf("the refusal is counted:\n%s", metrics)
	}
}

func TestTheNodeRefusesAMessageThatCarriesOneOfItsOwnTokens(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	tokens := mustTokens(t, tokA+"\n"+tokB+"\nlaptop-a:"+tokM+"\n")
	srv := httptest.NewServer(NewHandler(s, "", WithTokenSet(tokens), WithObserverToken(tokO)))
	defer srv.Close()
	// none of these has a credential shape: the node knows them exactly
	for name, tok := range map[string]string{"shared": tokA, "second shared": tokB, "machine": tokM, "observer": tokO} {
		payload, _ := json.Marshal(map[string]string{"from": "a/b", "to": "all", "body": "the token is " + tok + " (copied from the config)"})
		code, body := call(t, srv, tokA, "POST", "/v1/send", string(payload))
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "token this mesh accepts") || !strings.Contains(body, "say where the secret lives") {
			t.Errorf("%s token: %d %q", name, code, body)
		}
		if strings.Contains(body, tok) {
			t.Errorf("%s token: the refusal must not repeat the secret: %q", name, body)
		}
	}
	if n := len(s.Inbox("z/z", true)); n != 0 {
		t.Errorf("nothing may be stored: %d messages", n)
	}
	// near misses and ordinary text still go through, and a machine token sends as itself
	for _, text := range []string{
		"the token ends in " + tokA[len(tokA)-8:],                  // a fragment is not the token
		"a different token " + strings.ToUpper(tokA),               // case matters
		"tokens are in ~/.config/aivoicebus/token on each machine", // the place, not the secret
	} {
		payload, _ := json.Marshal(map[string]string{"from": "a/b", "to": "all", "body": text})
		if code, body := call(t, srv, tokA, "POST", "/v1/send", string(payload)); code != 200 {
			t.Errorf("%q: %d %q", text, code, body)
		}
	}
	if code, body := call(t, srv, tokM, "POST", "/v1/send", `{"from":"laptop-a/claude.1","to":"all","body":"hello"}`); code != 200 {
		t.Errorf("a machine token still sends as its machine: %d %q", code, body)
	}
	_, metrics := call(t, srv, tokA, "GET", "/metrics", "")
	if !strings.Contains(metrics, "aivoicebus_secrets_refused_total 4") {
		t.Errorf("each refusal is counted with the existing counter:\n%s", metrics)
	}
}

func TestATokenTooShortToBeDistinctiveIsNeverACanary(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, "tok")) // a test node's token: shorter than any real one
	defer srv.Close()
	if code, body := call(t, srv, "tok", "POST", "/v1/send", `{"from":"a/b","to":"all","body":"a token is a tokens"}`); code != 200 {
		t.Errorf("%d %q", code, body)
	}
}

func TestHealthAdvertisesFeaturesAndAClientCanAskForThem(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA, WithObserverToken(tokO)))
	defer srv.Close()
	c := &client.Client{URLs: []string{srv.URL}, Token: tokO}
	h, err := c.Health()
	if err != nil {
		t.Fatal(err)
	}
	if !h.Has(msg.FeatureRetract) || !h.Has(msg.FeatureQuarantine) || h.Has("no-such-feature") {
		t.Errorf("features %v", h.Features)
	}
}
