package server

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

func TestANodeCleansTitlesNamesTheSenderAndNeverStoresACredentialInOne(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA))
	defer srv.Close()

	code, body := call(t, srv, tokA, "POST", "/v1/send", `{"from":"h/claude.a1b2","to":"all","body":"hi","from_title":"Fix the\nlogin‮"}`)
	var m msg.Message
	if code != 200 || json.Unmarshal([]byte(body), &m) != nil || m.FromTitle != "Fix the login" {
		t.Fatalf("%d %s", code, body)
	}
	// the sender is in the roster under that name, though it never read
	_, body = call(t, srv, tokA, "GET", "/v1/roster", "")
	if !strings.Contains(body, `"title":"Fix the login"`) {
		t.Errorf("roster: %s", body)
	}
	// a title shaped like a credential is dropped; the message still goes
	tok := "gh" + "p_" + strings.Repeat("aB3dE5fG7h", 4)[:36]
	code, body = call(t, srv, tokA, "POST", "/v1/send", `{"from":"h/claude.c3d4","to":"all","body":"hi","from_title":"use `+tok+`"}`)
	if code != 200 || strings.Contains(body, tok) || strings.Contains(body, "from_title") {
		t.Errorf("%d %s", code, body)
	}
	if strings.Contains(s.Roster()[0].Title+s.Roster()[1].Title, tok) {
		t.Error("a credential reached the roster")
	}
}

func TestAnInboxReadDeclaresTheSessionsTitle(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA))
	defer srv.Close()
	code, _ := call(t, srv, tokA, "GET", "/v1/inbox?me=h/claude.a1b2&"+msg.ParamTitle+"="+url.QueryEscape("Review the\tparser"), "")
	if code != 200 {
		t.Fatal(code)
	}
	_, body := call(t, srv, tokA, "GET", "/v1/roster", "")
	if !strings.Contains(body, `"title":"Review the parser"`) {
		t.Errorf("%s", body)
	}
	// a read without the parameter keeps the name
	call(t, srv, tokA, "GET", "/v1/inbox?me=h/claude.a1b2", "")
	_, body = call(t, srv, tokA, "GET", "/v1/roster", "")
	if !strings.Contains(body, `"title":"Review the parser"`) {
		t.Errorf("forgotten: %s", body)
	}
}
