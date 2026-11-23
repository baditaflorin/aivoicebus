package sound

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A Mac that is in a Focus (Do Not Disturb, Sleep, Personal, Work, a custom one) is a Mac whose person asked
// not to be disturbed, so the trackpad stays still. macOS keeps the Focus modes that are on in
// ~/Library/DoNotDisturb/DB/Assertions.json: one record per assertion that is on (turning a Focus on, by hand,
// by schedule or from another device that shares its Focus, adds one; turning it off moves it to a list of
// invalidated ones), which is the whole state, read without a framework or a helper. Only the identifiers of
// the modes are read: never the Focus configurations, which name the people and apps each one lets through.

// focusFile is where the Mac keeps it (a variable so tests can point it elsewhere).
var focusFile = func() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(h, "Library", "DoNotDisturb", "DB", "Assertions.json")
}

const maxFocusFile = 8 << 20 // the file is about 10 KB; this only keeps a runaway one from filling memory

// Modes the system names by identifier, as the person sees them in Control Center. Any other identifier
// (a Focus the person made, or one a later macOS adds) still counts as a Focus and is named by its last part.
var focusNames = map[string]string{
	"com.apple.donotdisturb.mode.default":  "Do Not Disturb",
	"com.apple.sleep.sleep-mode":           "Sleep",
	"com.apple.focus.personal-time":        "Personal",
	"com.apple.focus.work":                 "Work",
	"com.apple.donotdisturb.mode.driving":  "Driving",
	"com.apple.focus.driving":              "Driving",
	"com.apple.focus.fitness":              "Fitness",
	"com.apple.focus.gaming":               "Gaming",
	"com.apple.focus.mindfulness":          "Mindfulness",
	"com.apple.focus.reading":              "Reading",
	"com.apple.focus.reduce-interruptions": "Reduce Interruptions",
}

type focusState struct {
	Data []struct {
		Active []struct {
			UUID    string `json:"assertionUUID"`
			Details struct {
				Mode string `json:"assertionDetailsModeIdentifier"`
			} `json:"assertionDetails"`
		} `json:"storeAssertionRecords"`
	} `json:"data"`
}

// ActiveFocus names the Focus modes that are on now, sorted and without repeats; none when the Mac is not in
// one, is not a Mac, or has never used Focus (no file). The error says why the state could not be read: the
// file is private to the person, and a terminal or app without Full Disk Access may be refused it. Whoever
// asks decides what an unknown state means; Play treats it as no Focus, and `aivoicebus sound` says so.
func ActiveFocus() ([]string, error) {
	if goos != "darwin" {
		return nil, nil
	}
	p := focusFile()
	if p == "" {
		return nil, nil
	}
	f, err := os.Open(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case errors.Is(err, fs.ErrPermission):
		return nil, errors.New("this process may not read the Focus state (macOS keeps it private: give the terminal or app that runs aivoicebus Full Disk Access in System Settings > Privacy & Security, or turn the check off with `aivoicebus sound focus ignore`)")
	case err != nil:
		return nil, fmt.Errorf("cannot read the Focus state: %v", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("the Focus state is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFocusFile+1))
	if err != nil {
		return nil, fmt.Errorf("cannot read the Focus state: %v", err)
	}
	if len(b) > maxFocusFile {
		return nil, errors.New("the Focus state file is larger than expected")
	}
	var st focusState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("cannot understand the Focus state (a macOS release that keeps it differently?): %v", err)
	}
	seen := map[string]bool{}
	var names []string
	for _, blk := range st.Data {
		for _, a := range blk.Active {
			if a.UUID != "" && seen[a.UUID] { // the file lists an assertion twice when two stores hold it
				continue
			}
			seen[a.UUID] = true
			if n := focusName(a.Details.Mode); !contains(names, n) {
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	return names, nil
}

func focusName(mode string) string {
	if n, ok := focusNames[mode]; ok {
		return n
	}
	if i := strings.LastIndex(mode, "."); i >= 0 && i < len(mode)-1 {
		return mode[i+1:]
	}
	if mode == "" {
		return "a Focus"
	}
	return mode
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
