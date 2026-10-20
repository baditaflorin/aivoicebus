package msg

import (
	"strings"
	"unicode"
)

// How the nodes tell a conversation that goes somewhere from one that repeats itself (the loop
// check and the pair brake, store.turnsLeftLocked and store.allowSendLocked). A message is a repeat
// when its words are at least RepeatSimilarity the same as one of the sender's last RepeatLookback
// messages in the same thread, within the hour: Similarity, the Jaccard index of the two bodies'
// words and word pairs, with case and punctuation aside. Numbers are words like any other, so
// "turn 7" after "turn 6" is new, but a long message sent again with one number changed is not.
const (
	RepeatSimilarity = 0.6
	RepeatLookback   = 8
	// BrakesSince is the release whose nodes judge repeats this way (feature brakes). Older nodes
	// refuse the same words said a third time, numbers aside, and every 24th fast reply.
	BrakesSince   = "0.12.0"
	FeatureBrakes = "brakes"
)

// similarText is how much of a body Shingles reads: a repeat shows in its first few kilobytes.
const similarText = 8 << 10

// Shingles is the set of a body's words and adjacent word pairs, lowercased, punctuation aside.
// A word is a run of letters or digits.
func Shingles(body string) map[string]struct{} {
	if len(body) > similarText {
		body = body[:similarText]
	}
	words := strings.FieldsFunc(strings.ToLower(body), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	set := make(map[string]struct{}, 2*len(words))
	for i, w := range words {
		set[w] = struct{}{}
		if i > 0 {
			set[words[i-1]+" "+w] = struct{}{}
		}
	}
	return set
}

// Jaccard is how alike two shingle sets are, 0 (nothing shared) to 1 (the same). Two empty sets
// (bodies of only punctuation, "!!!" and "?") are the same.
func Jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	both := 0
	for s := range a {
		if _, ok := b[s]; ok {
			both++
		}
	}
	return float64(both) / float64(len(a)+len(b)-both)
}

// Similarity is how alike two bodies are, 0 to 1 (see Shingles and Jaccard).
func Similarity(a, b string) float64 { return Jaccard(Shingles(a), Shingles(b)) }
