package outbox

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A relative time in a message ("in about 11 h", "within 2 hours") is relative to when the reader
// opens it, which is often hours after it was sent: the review of a week of the hub's messages found
// deadlines read a day late and an all-clear that pointed at the wrong time. LintRelativeTimes says
// where the text does that and what the same moment is as a UTC clock time at the send. It only
// warns: rewriting the sender's words would change a duration ("takes about 5 min") into a deadline
// nobody wrote, in a message that may be signed or sealed.
var relativeTime = regexp.MustCompile(`(?i)\b(in|within)\s+(?:about\s+|around\s+|roughly\s+|approx\.?\s+|approximately\s+|~\s*)?(\d+(?:\.\d+)?)\s*(minutes?|mins?|m|hours?|hrs?|h|days?|d)\b`)

// maxRelativeShown is how many phrases are named in the warning before it counts the rest.
const maxRelativeShown = 3

// LintRelativeTimes is one warning naming the relative times body uses, with the UTC time each would be
// if the message were read now, or "" when it uses none. Spans over 30 days are ignored (a duration,
// not a time the reader waits for).
func LintRelativeTimes(body string, now time.Time) string {
	now = now.UTC()
	var named []string
	seen := map[string]bool{}
	total := 0
	for _, m := range relativeTime.FindAllStringSubmatch(body, -1) {
		n, err := strconv.ParseFloat(m[2], 64)
		if err != nil || n <= 0 {
			continue
		}
		var unit time.Duration
		switch strings.ToLower(m[3])[0] {
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		default:
			unit = 24 * time.Hour
		}
		d := time.Duration(n * float64(unit))
		if d > 30*24*time.Hour {
			continue
		}
		phrase := strings.Join(strings.Fields(m[0]), " ")
		if seen[strings.ToLower(phrase)] {
			continue
		}
		seen[strings.ToLower(phrase)] = true
		total++
		if len(named) < maxRelativeShown {
			at := now.Add(d).Round(time.Minute)
			shown := at.Format("15:04") + "Z"
			if at.YearDay() != now.YearDay() || at.Year() != now.Year() {
				shown = at.Format("2006-01-02 15:04") + "Z"
			}
			named = append(named, fmt.Sprintf("%q is %s", phrase, shown))
		}
	}
	if total == 0 {
		return ""
	}
	if more := total - len(named); more > 0 {
		named = append(named, fmt.Sprintf("and %d more", more))
	}
	return fmt.Sprintf("warning: the text says %s (sent at %sZ). A message is often read hours later, when \"in 11 h\" points at the wrong time: write the time itself (04:15Z). Nothing was changed.",
		strings.Join(named, ", "), now.Format("15:04"))
}
