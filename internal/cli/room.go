package cli

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

// A room is a conversation of several agents that stays put: its members are the sessions that
// joined it (a rooms label they declare on every read, like capabilities), everything said in it
// goes to @room=NAME, and all of it is one thread, room-NAME, so `room wait` and the viewer follow it.

var reRoom = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,39}$`)

func roomThread(name string) string { return "room-" + name }

func roomsOf(me string) []string {
	var out []string
	for _, r := range strings.Split(config.Rooms(me), "+") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func roomCmd(args []string, s Streams) error {
	usage := outbox.Usagef(`usage: room join|leave|say|wait|who|invite|list ...   e.g. room join release-0.5; room say release-0.5 "text" [--wait-reply 10m]; room wait release-0.5 [--for 10m]; room invite release-0.5 laptop-b/codex`)
	if len(args) == 0 {
		return usage
	}
	me := identity.Me()
	sub, rest := args[0], args[1:]
	if sub == "list" {
		rs := roomsOf(me)
		if len(rs) == 0 {
			fmt.Fprintln(s.Out, "(this session is in no room; join one with: aivoicebus room join NAME)")
		}
		for _, r := range rs {
			fmt.Fprintln(s.Out, r)
		}
		return nil
	}
	if len(rest) == 0 || !reRoom.MatchString(strings.ToLower(rest[0])) {
		return outbox.Usagef("a room name is 1 to 40 lowercase letters, digits and . _ - (starting with a letter or digit)")
	}
	name := strings.ToLower(rest[0])
	rest = rest[1:]
	switch sub {
	case "join":
		rs := roomsOf(me)
		for _, r := range rs {
			if r == name {
				fmt.Fprintf(s.Out, "already in %s\n", name)
				return nil
			}
		}
		next := msg.CleanCaps(strings.Join(append(rs, name), "+"))
		if !strings.Contains("+"+next+"+", "+"+name+"+") {
			return fmt.Errorf("too many rooms for one session: leave one first (aivoicebus room leave NAME)")
		}
		config.SaveRooms(me, next)
		c, err := sessionClient()
		if err != nil {
			return err
		}
		_, _ = c.Inbox(me, false) // declare the label now, so the room reaches this session at once
		fmt.Fprintf(s.Out, "joined %s: messages to @room=%s reach this session; aivoicebus room wait %s follows it\n", name, name, name)
		return nil
	case "leave":
		var keep []string
		found := false
		for _, r := range roomsOf(me) {
			if r == name {
				found = true
				continue
			}
			keep = append(keep, r)
		}
		if !found {
			return outbox.NotFoundf("this session is not in %s", name)
		}
		config.SaveRooms(me, strings.Join(keep, "+"))
		if c, err := sessionClient(); err == nil {
			_, _ = c.Inbox(me, false)
		}
		fmt.Fprintf(s.Out, "left %s\n", name)
		return nil
	case "say":
		return sendWith(append([]string{"--to", "@room=" + name, "--thread", roomThread(name), "--queue"}, rest...), s, false)
	case "wait":
		return waitCmd(append([]string{"--thread", roomThread(name)}, rest...), s)
	case "invite":
		if len(rest) < 1 {
			return outbox.Usagef("usage: room invite NAME TARGET")
		}
		return sendWith([]string{"--to", rest[0], "--thread", roomThread(name),
			fmt.Sprintf("Invitation to the room %s (a conversation of several agents). To join: aivoicebus room join %s, then aivoicebus room wait %s. Ask your user first.", name, name, name)}, s, false)
	case "who":
		c, err := sessionClient()
		if err != nil {
			return err
		}
		roster, err := c.Roster()
		if err != nil {
			return err
		}
		sort.Slice(roster, func(i, j int) bool { return roster[i].Me < roster[j].Me })
		n := 0
		now := time.Now()
		for _, e := range roster {
			if !strings.Contains("+"+e.Labels[msg.RoomsLabel]+"+", "+"+name+"+") {
				continue
			}
			n++
			state := "seen " + now.Sub(e.Seen).Round(time.Second).String() + " ago"
			if e.Listening {
				state = "listening now"
			}
			fmt.Fprintf(s.Out, "%-40s %s%s\n", view.Name(e.Me, msg.MaxIdentity), state, view.TitleSuffix(e.Title))
		}
		if n == 0 {
			fmt.Fprintf(s.Out, "nobody is in %s (yet)\n", name)
		}
		return nil
	}
	return usage
}
