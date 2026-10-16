package msg

import (
	"strings"
	"testing"
)

func TestCleanModel(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5":       "claude-opus-5-5",
		" Claude-Sonnet-5-5 ":   "claude-sonnet-5-5",
		"gpt-5.1-codex":         "gpt-5.1-codex",
		"<synthetic>":           "",
		"":                      "",
		"two words":             "",
		"model/../x":            "",
		strings.Repeat("a", 65): "",
	} {
		if got := CleanModel(in); got != want {
			t.Errorf("CleanModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReplicatedRecordsBoundTheModel(t *testing.T) {
	r := Record{Origin: "n1", Seq: 1, Msg: &Message{ID: "0123456789", From: "h/a", To: "all", Thread: "0123456789", Body: "x", FromModel: "claude-opus-5-5"}}
	if err := r.ValidateReplicated(); err != nil {
		t.Errorf("an ordinary model: %v", err)
	}
	r.Msg.FromModel = strings.Repeat("m", replMaxModel+1)
	if err := r.ValidateReplicated(); err == nil || !strings.Contains(err.Error(), "from_model") {
		t.Errorf("%v", err)
	}
}
