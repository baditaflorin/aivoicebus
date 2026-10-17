package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// Scheduled tasks: recurring work with no person and no cron. `schedule add` keeps them in
// ~/.config/aivoicebus/schedules.json; the worker (or `schedule run`) sends each as a task when it
// is due, from this machine, marked "[scheduled NAME]".

var scheduleName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

const (
	scheduleFile = "schedules.json"
	maxSchedules = 20
	minEvery     = 5 * time.Minute
)

type schedule struct {
	Name  string        `json:"name"`
	To    string        `json:"to"`
	Text  string        `json:"text"`
	Every time.Duration `json:"every,omitempty"` // every this long, or
	At    string        `json:"at,omitempty"`    // daily at HH:MM local time
	Last  time.Time     `json:"last,omitempty"`  // when it was last sent
	Added time.Time     `json:"added"`
}

// due reports whether sc should be sent at now.
func (sc schedule) due(now time.Time) bool {
	if sc.Every > 0 {
		from := sc.Last
		if from.IsZero() {
			from = sc.Added
		}
		return !now.Before(from.Add(sc.Every))
	}
	hh, mm, ok := parseHHMM(sc.At)
	if !ok {
		return false
	}
	l := now.Local()
	today := time.Date(l.Year(), l.Month(), l.Day(), hh, mm, 0, 0, time.Local)
	return !l.Before(today) && sc.Last.Before(today) && sc.Added.Before(today)
}

func parseHHMM(s string) (int, int, bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

func loadSchedules() ([]schedule, error) {
	b, err := os.ReadFile(filepath.Join(config.Dir(), scheduleFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []schedule
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", scheduleFile, err)
	}
	return out, nil
}

func saveSchedules(ss []schedule) error {
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(ss, "", "  ")
	p := filepath.Join(config.Dir(), scheduleFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// runSchedules sends every due schedule as a task and records when; it says what it sent.
func runSchedules(c *client.Client, now time.Time, out io.Writer) (int, error) {
	ss, err := loadSchedules()
	if err != nil || len(ss) == 0 {
		return 0, err
	}
	sent := 0
	for i, sc := range ss {
		if !sc.due(now) {
			continue
		}
		// From the machine's scheduler, not the worker: a node shows nobody their own messages, so a
		// task the worker sent as itself never reached it (found live), and a receiving allowlist
		// can name `<machine>/scheduler` on its own.
		plan, err := outbox.Prepare(c, outbox.Request{To: sc.To, Kind: msg.KindTask, Body: "[scheduled " + sc.Name + "] " + sc.Text, Queue: true, TargetSaid: true,
			From: msg.HostOf(identity.Me()) + "/scheduler"}, io.Discard)
		if err == nil {
			var m msg.Message
			if m, err = plan.Send(c); err == nil {
				fmt.Fprintf(out, "schedule %s: sent task %s to %s\n", sc.Name, m.ID, msg.SafeName(sc.To))
				sent++
			}
		}
		if err != nil {
			fmt.Fprintf(out, "schedule %s: not sent: %v\n", sc.Name, err)
			if client.IsUnreachable(err) {
				return sent, err // tried again next time: Last stays
			}
		}
		ss[i].Last = now // a schedule that cannot be sent (refused) is not retried every minute
	}
	return sent, saveSchedules(ss)
}

func scheduleCmd(args []string, s Streams) error {
	usage := outbox.Usagef(`usage: schedule add NAME (--every 6h | --at 09:00) MACHINE "what to do" [--tool codex]   |   schedule list | rm NAME | run [--once]`)
	if len(args) == 0 {
		return usage
	}
	switch args[0] {
	case "list":
		ss, err := loadSchedules()
		if err != nil {
			return err
		}
		if len(ss) == 0 {
			fmt.Fprintln(s.Out, "no schedules (aivoicebus schedule add ...); the worker sends them when they are due")
			return nil
		}
		for _, sc := range ss {
			when := "every " + sc.Every.String()
			if sc.At != "" {
				when = "daily at " + sc.At
			}
			last := "never yet"
			if !sc.Last.IsZero() {
				last = "last " + sc.Last.Local().Format("01-02 15:04")
			}
			fmt.Fprintf(s.Out, "%-16s %-16s to %-28s %s: %s\n", sc.Name, when, msg.SafeName(sc.To), last, sc.Text)
		}
		return nil
	case "rm":
		if len(args) != 2 {
			return usage
		}
		ss, err := loadSchedules()
		if err != nil {
			return err
		}
		n := len(ss)
		ss = slicesDelete(ss, func(sc schedule) bool { return sc.Name == args[1] })
		if len(ss) == n {
			return outbox.NotFoundf("no schedule named %q", args[1])
		}
		if err := saveSchedules(ss); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "removed schedule %s\n", args[1])
		return nil
	case "run":
		c, err := client.FromEnv()
		if err != nil {
			return err
		}
		n, err := runSchedules(c, time.Now(), s.Out)
		if err == nil && n == 0 {
			fmt.Fprintln(s.Out, "nothing is due")
		}
		return err
	case "add":
		return scheduleAdd(args[1:], s, usage)
	}
	return usage
}

func scheduleAdd(args []string, s Streams, usage error) error {
	var name, at, tool string
	var every time.Duration
	var words []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		key, val, hasEq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") {
			if !hasEq {
				if i+1 >= len(args) {
					return usage
				}
				i++
				val = args[i]
			}
			switch key {
			case "every":
				d, err := time.ParseDuration(val)
				if err != nil {
					return outbox.Usagef("--every: %v", err)
				}
				every = d
			case "at":
				at = val
			case "tool":
				tool = val
			default:
				return usage
			}
			continue
		}
		words = append(words, a)
	}
	if len(words) < 3 {
		return usage
	}
	name, machine, text := words[0], words[1], strings.Join(words[2:], " ")
	switch {
	case !scheduleName.MatchString(name):
		return outbox.Usagef("the name is one word: letters, digits, - or _")
	case (every > 0) == (at != ""):
		return outbox.Usagef("give --every DURATION or --at HH:MM, one of them")
	case every > 0 && every < minEvery:
		return outbox.Usagef("--every is at least %s", minEvery)
	}
	if at != "" {
		if _, _, ok := parseHHMM(at); !ok {
			return outbox.Usagef("--at is HH:MM, 24-hour local time")
		}
	}
	to, err := taskTarget(machine, tool, "", "")
	if err != nil {
		return outbox.Usagef("%v", err)
	}
	ss, err := loadSchedules()
	if err != nil {
		return err
	}
	if len(ss) >= maxSchedules {
		return fmt.Errorf("%d schedules already: remove one first", maxSchedules)
	}
	for _, sc := range ss {
		if sc.Name == name {
			return outbox.Usagef("a schedule named %q exists: schedule rm %s first", name, name)
		}
	}
	ss = append(ss, schedule{Name: name, To: to, Text: text, Every: every, At: at, Added: time.Now().UTC()})
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].Name < ss[j].Name })
	if err := saveSchedules(ss); err != nil {
		return err
	}
	when := "every " + every.String()
	if at != "" {
		when = "daily at " + at
	}
	fmt.Fprintf(s.Out, "schedule %s: a task to %s %s. A worker on this machine sends it when it is due (aivoicebus worker install ...), or: aivoicebus schedule run\n", name, msg.SafeName(to), when)
	return nil
}

func slicesDelete(ss []schedule, del func(schedule) bool) []schedule {
	out := ss[:0]
	for _, sc := range ss {
		if !del(sc) {
			out = append(out, sc)
		}
	}
	return out
}
