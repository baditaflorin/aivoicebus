package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
)

// The agent CLIs say what a run cost when asked for machine-readable output: `codex exec --json`
// prints JSON lines (the answer in item.completed agent_message, the tokens in turn.completed),
// and `claude -p --output-format json` prints one object (result, usage, total_cost_usd). The
// worker asks its presets for that, takes the answer text out, and keeps the counts.

// Output formats a preset's command prints.
const (
	FormatText       = ""            // the answer as plain text
	FormatCodexJSONL = "codex-jsonl" // codex exec --json
	FormatClaudeJSON = "claude-json" // claude -p --output-format json
)

// Usage is what one run cost, as the agent's CLI reported it.
type Usage struct {
	Input     int     `json:"input_tokens"`
	Cached    int     `json:"cached_input_tokens,omitempty"`
	Output    int     `json:"output_tokens"`
	Reasoning int     `json:"reasoning_tokens,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// Total is every token counted, input (cached included) and output.
func (u Usage) Total() int { return u.Input + u.Output + u.Reasoning }

// Line is the usage as a few words for a person: "14627 tokens in (14000 cached), 5 out, $0.0123".
func (u Usage) Line() string {
	s := fmt.Sprintf("%d tokens in", u.Input)
	if u.Cached > 0 {
		s += fmt.Sprintf(" (%d cached)", u.Cached)
	}
	s += fmt.Sprintf(", %d out", u.Output+u.Reasoning)
	if u.CostUSD > 0 {
		s += fmt.Sprintf(", $%.4f", u.CostUSD)
	}
	return s
}

// ParseOutput takes the answer text and the usage out of what a command printed in format. A
// format it cannot read, or text it does not recognise, comes back as it was, with no usage.
func ParseOutput(format, out string) (string, *Usage) {
	switch format {
	case FormatCodexJSONL:
		var text []string
		var u Usage
		seen := false
		sc := bufio.NewScanner(strings.NewReader(out))
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			var ev struct {
				Type string `json:"type"`
				Item struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"item"`
				Usage struct {
					Input     int `json:"input_tokens"`
					Cached    int `json:"cached_input_tokens"`
					Output    int `json:"output_tokens"`
					Reasoning int `json:"reasoning_output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			switch {
			case ev.Type == "item.completed" && ev.Item.Type == "agent_message":
				text = append(text, ev.Item.Text)
			case ev.Type == "turn.completed":
				u.Input += ev.Usage.Input
				u.Cached += ev.Usage.Cached
				u.Output += ev.Usage.Output
				u.Reasoning += ev.Usage.Reasoning
				seen = true
			}
		}
		if !seen && len(text) == 0 {
			return out, nil
		}
		if !seen {
			return strings.Join(text, "\n"), nil
		}
		return strings.Join(text, "\n"), &u
	case FormatClaudeJSON:
		var r struct {
			Result string  `json:"result"`
			Cost   float64 `json:"total_cost_usd"`
			Usage  struct {
				Input       int `json:"input_tokens"`
				CacheRead   int `json:"cache_read_input_tokens"`
				CacheCreate int `json:"cache_creation_input_tokens"`
				Output      int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(out)), &r) != nil {
			return out, nil
		}
		u := Usage{Input: r.Usage.Input + r.Usage.CacheRead + r.Usage.CacheCreate, Cached: r.Usage.CacheRead, Output: r.Usage.Output, CostUSD: r.Cost}
		return r.Result, &u
	}
	return out, nil
}
