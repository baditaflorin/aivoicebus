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

// A read that declares a title is weighed with it: a new interactive session's first read (its session
// start) is offered a machine task as an interactive one, not as an untitled run.
func TestAReadIsOfferedATaskWithTheTitleItDeclares(t *testing.T) {
	s, _ := store.Open(t.TempDir(), "n1")
	srv := httptest.NewServer(NewHandler(s, tokA))
	defer srv.Close()
	title := "&" + msg.ParamTitle + "=" + url.QueryEscape("release work")
	call(t, srv, tokA, "GET", "/v1/inbox?me=lap/claude.x1"+title, "") // an interactive session is about
	code, body := call(t, srv, tokA, "POST", "/v1/send", `{"from":"desk/claude.z9","to":"lap","kind":"task","body":"roll it out","offer":{"left":9}}`)
	var task msg.Message
	if code != 200 || json.Unmarshal([]byte(body), &task) != nil || task.Offer != nil {
		t.Fatalf("%d %s", code, body)
	}
	_, body = call(t, srv, tokA, "GET", "/v1/inbox?me=lap/claude.t2"+title, "")
	var got []msg.Message
	if json.Unmarshal([]byte(body), &got) != nil || len(got) != 1 || got[0].ID != task.ID || got[0].Offer == nil || got[0].Offer.Left != 1 {
		t.Fatalf("a titled session's first read is not offered the task: %s", body)
	}
	if _, body = call(t, srv, tokA, "GET", "/v1/inbox?me=lap/claude.x1", ""); strings.Contains(body, task.ID) {
		t.Errorf("the other session is shown the task while the first holds its offer: %s", body)
	}
	_, body = call(t, srv, tokA, "GET", "/v1/receipts?id="+task.ID, "")
	if !strings.Contains(body, `"offers":[{"by":"lap/claude.t2"`) || !strings.Contains(body, `"outcome":"open"`) {
		t.Errorf("receipts do not list the offer: %s", body)
	}
	_, body = call(t, srv, tokA, "GET", "/v1/health", "")
	if !strings.Contains(body, `"`+msg.FeatureOffers+`"`) {
		t.Errorf("the node does not advertise %s: %s", msg.FeatureOffers, body)
	}
}
