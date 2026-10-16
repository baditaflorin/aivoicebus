package client

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

func TestTheClientDeclaresTheSessionsTitleAndSendsASnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(config.TitleEnv, "")
	config.SaveTitle("h/claude.a1b2", "Fix the\nlogin")
	config.SaveModel("h/claude.a1b2", "Claude-Opus-5-5")
	var inboxTitle, sentTitle, sentModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/inbox":
			inboxTitle = r.URL.Query().Get(msg.ParamTitle)
			w.Write([]byte("[]"))
		case "/v1/send":
			b, _ := io.ReadAll(r.Body)
			var m msg.Message
			json.Unmarshal(b, &m)
			sentTitle, sentModel = m.FromTitle, m.FromModel
			w.Write(b)
		}
	}))
	defer srv.Close()
	c := &Client{URLs: []string{srv.URL}, Token: "t", Warn: io.Discard}
	if _, err := c.Inbox("h/claude.a1b2", false); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Send(msg.Message{From: "h/claude.a1b2", To: "all", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if inboxTitle != "Fix the login" || sentTitle != "Fix the login" || sentModel != "claude-opus-5-5" {
		t.Errorf("inbox %q, send %q, model %q", inboxTitle, sentTitle, sentModel)
	}
	// another session on the machine has its own (none here), and AIVOICEBUS_TITLE=off sends none
	sentTitle = "unset"
	c.Send(msg.Message{From: "h/codex.q1w2", To: "all", Body: "x"})
	if sentTitle != "" {
		t.Errorf("another session's title was used: %q", sentTitle)
	}
	t.Setenv(config.TitleEnv, "off")
	c.Inbox("h/claude.a1b2", false)
	if inboxTitle != "" {
		t.Errorf("off: %q", inboxTitle)
	}
	if strings.Contains(sentTitle, "\n") {
		t.Error("an uncleaned title was sent")
	}
}
