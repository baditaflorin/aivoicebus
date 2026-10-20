package msg

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ThreadSize is what a conversation has cost so far, as either side can count it from the thread:
// its messages, the characters of their bodies (about four a token, what reading it all again
// costs an agent), and the tokens agents reported for runs in it (a worker's answer header,
// "(N tokens in, M out)", the counts `aivoicebus tokens` adds up). Display only: anyone can write a
// header like that.
type ThreadSize struct {
	Messages  int     `json:"messages"`
	Chars     int     `json:"chars"`
	EstTokens int     `json:"est_tokens"` // Chars / CharsPerToken
	Runs      int     `json:"runs_with_counts,omitempty"`
	TokensIn  int     `json:"tokens_in,omitempty"`
	TokensOut int     `json:"tokens_out,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// CharsPerToken is the rough rule ThreadSize estimates tokens by.
const CharsPerToken = 4

var reportedTokens = regexp.MustCompile(`^\[worker on [^\]]+\] exit -?\d+ after [0-9a-zµ.]+ \((\d+) tokens in(?: \(\d+ cached\))?, (\d+) out(?:, \$([0-9.]+))?\)`)

// SizeOf adds up ms (one thread). A message counts once however many copies there are.
func SizeOf(ms []Message) ThreadSize {
	var z ThreadSize
	seen := map[string]bool{}
	for _, m := range ms {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		z.Messages++
		z.Chars += utf8.RuneCountInString(m.Body)
		if _, agent, _ := strings.Cut(m.From, "/"); !strings.HasSuffix(agent, "worker") || m.Retracted != nil {
			continue
		}
		head, _, _ := strings.Cut(m.Body, "\n")
		if g := reportedTokens.FindStringSubmatch(head); g != nil {
			in, _ := strconv.Atoi(g[1])
			out, _ := strconv.Atoi(g[2])
			cost, _ := strconv.ParseFloat(g[3], 64)
			z.Runs++
			z.TokensIn += in
			z.TokensOut += out
			z.CostUSD += cost
		}
	}
	z.EstTokens = z.Chars / CharsPerToken
	return z
}
