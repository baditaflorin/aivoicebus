package client

import (
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/sound"
)

// The taps a person can ask for on the trackpad (`aivoicebus sound`) are asked for here because every command, hook,
// MCP tool and worker reaches the nodes through a Client: one place hears a send, a retraction and
// the mail that arrives. All of it is a no-op until the person turned sounds on, and none of it can
// fail a call.

// The two calls into the sound package, variables so a test can listen instead of tap.
var (
	soundsOn  = sound.Enabled
	playSound = sound.Play
)

// heardAs is the name the messages already announced by a tap are recorded under (config.Told):
// one per machine, so two sessions reading one message make one tap.
const heardAs = "sound-received"

// protocol reports whether m is the bus talking to itself (a claim, a lease), not a message a person
// would count as sent or received.
func protocol(m msg.Message) bool { return m.Kind == msg.KindClaim || m.Kind == msg.KindLease }

// soundSent is the sound of m having gone out. A test-lane message (selftest) and the throwaway probe
// of `join` and `selftest` make none: they are the bus checking itself.
func (c *Client) soundSent(m msg.Message) {
	if c.Lane || m.IsTest() || protocol(m) || msg.IsProbe(m.From) {
		return
	}
	playSound(sound.Sent, false)
}

// soundRetracted is the sound of a message of this machine taken back, by the identity by.
func soundRetracted(by string) {
	if !msg.IsProbe(by) {
		playSound(sound.Retracted, false)
	}
}

// soundReceived makes one sound for the unread messages ms that session me was just handed and
// this machine has not made a sound for before. Messages are marked as they are counted, in quiet
// hours too, so one that arrived then does not sound when they end.
func soundReceived(me string, ms []msg.Message) {
	if len(ms) == 0 || msg.IsProbe(me) || !soundsOn() {
		return
	}
	fresh, urgent := false, false
	for _, m := range ms {
		if m.From == me || m.Retracted != nil || protocol(m) || config.Told(heardAs, m.ID) {
			continue
		}
		config.NoteTold(heardAs, m.ID)
		fresh = true
		urgent = urgent || m.Prio == msg.PrioUrgent
	}
	if fresh {
		playSound(sound.Received, urgent)
	}
}
