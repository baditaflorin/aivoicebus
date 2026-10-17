package worker

import "testing"

// Codex's --json output as it printed it on a Mac (codex-cli 0.162), and Claude Code's documented
// --output-format json.
func TestParseOutputTakesTheAnswerAndTheTokens(t *testing.T) {
	codex := `{"type":"thread.started","thread_id":"01a1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"57"}}
{"type":"turn.completed","usage":{"input_tokens":14627,"cached_input_tokens":14000,"cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":2}}
`
	text, u := ParseOutput(FormatCodexJSONL, codex)
	if text != "57" || u == nil || u.Input != 14627 || u.Cached != 14000 || u.Output != 5 || u.Reasoning != 2 || u.Total() != 14634 {
		t.Fatalf("codex: %q %+v", text, u)
	}
	if u.Line() != "14627 tokens in (14000 cached), 7 out" {
		t.Errorf("line: %q", u.Line())
	}
	claude := `{"type":"result","subtype":"success","result":"done: 3 files","total_cost_usd":0.0123,"usage":{"input_tokens":120,"cache_read_input_tokens":3000,"cache_creation_input_tokens":500,"output_tokens":42}}`
	text, u = ParseOutput(FormatClaudeJSON, claude)
	if text != "done: 3 files" || u == nil || u.Input != 3620 || u.Cached != 3000 || u.Output != 42 || u.CostUSD != 0.0123 {
		t.Fatalf("claude: %q %+v", text, u)
	}
	if text, u := ParseOutput(FormatClaudeJSON, "not json"); text != "not json" || u != nil {
		t.Errorf("unreadable output passes through: %q %+v", text, u)
	}
}
