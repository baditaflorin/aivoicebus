package agentcfg

import (
	"errors"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/assets"
)

const (
	beginMark = "<!-- aivoicebus:begin"
	endMark   = "<!-- aivoicebus:end -->"
)

// Block is the text placed in an agent tool's instruction file: the embedded
// snippet between markers that carry its content id.
func Block() string {
	return beginMark + " id=" + SnippetID() + " -->\n" + strings.TrimSpace(assets.Snippet()) + "\n" + endMark + "\n"
}

// ApplyBlock puts block into an instruction file's content. It replaces an
// existing marked block, or the section the original installer wrote (a
// "## aivoicebus:" heading up to the next "## " heading), or appends. Other
// content is untouched, and applying twice gives the same result.
//
// It refuses (a non-nil error, the content unchanged) when replacing would delete text it
// cannot vouch for: a begin marker with no end marker after it, or an old-style section
// that is more than the one paragraph the original installer wrote (the person added to it).
func ApplyBlock(cur, block string) (string, bool, error) {
	next, err := replaceBlock(cur, block)
	if err != nil {
		return cur, false, err
	}
	return next, next != cur, nil
}

func replaceBlock(cur, block string) (string, error) {
	if i := strings.Index(cur, beginMark); i >= 0 {
		j := strings.Index(cur[i:], endMark)
		if j < 0 {
			return "", errors.New("it has a aivoicebus begin marker with no end marker after it, so the block's extent is unknown")
		}
		end := i + j + len(endMark)
		if strings.HasPrefix(cur[end:], "\n") {
			end++
		}
		return cur[:i] + block + cur[end:], nil
	}
	if i := legacyStart(cur); i >= 0 {
		end := len(cur)
		if k := strings.Index(cur[i+1:], "\n## "); k >= 0 {
			end = i + 1 + k + 1
		}
		if !looksInstalled(cur[i:end]) {
			return "", errors.New("its old-style \"## aivoicebus:\" section holds more than the installer's one paragraph (headings or several paragraphs), so replacing it could delete your own text")
		}
		rest := cur[end:]
		if strings.TrimSpace(rest) != "" {
			rest = "\n" + rest
		} else {
			rest = ""
		}
		return cur[:i] + block + rest, nil
	}
	trimmed := strings.TrimRight(cur, "\n")
	if trimmed == "" {
		return block, nil
	}
	return trimmed + "\n\n" + block, nil
}

// looksInstalled says whether an old-style section is what the original installer wrote: the
// heading and one paragraph, with no further heading and no blank line inside it.
func looksInstalled(section string) bool {
	lines := strings.Split(strings.TrimSpace(section), "\n")
	text, gap := false, false
	for _, l := range lines[1:] {
		switch {
		case strings.HasPrefix(l, "#"):
			return false
		case strings.TrimSpace(l) == "":
			gap = text
		case gap: // text after a gap that followed text: a second paragraph
			return false
		default:
			text = true
		}
	}
	return true
}

func legacyStart(cur string) int {
	if strings.HasPrefix(cur, "## aivoicebus:") {
		return 0
	}
	if i := strings.Index(cur, "\n## aivoicebus:"); i >= 0 {
		return i + 1
	}
	return -1
}

// blockStatus says whether an instruction file has the current block.
func blockStatus(cur string) string {
	switch {
	case strings.Contains(cur, beginMark+" id="+SnippetID()+" -->") && strings.Contains(cur, endMark):
		return "current"
	case strings.Contains(cur, beginMark):
		return "stale"
	case legacyStart(cur) >= 0:
		return "legacy"
	default:
		return "missing"
	}
}
