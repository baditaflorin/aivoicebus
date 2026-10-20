package share

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
)

// MachineKeys are the keys machine host signs and seals with (base64): the pinned ones, else the
// ones in the most recent message host signed, read from the log (which needs the shared or
// observer token), which are then pinned. A signing key other than the pinned one is refused.
func MachineKeys(c *client.Client, host string) (sign, box string, err error) {
	if key, box, ok := config.KnownMachine(host); ok && box != "" {
		return key, box, nil
	}
	recs, err := c.SyncAll(map[string]uint64{})
	if err != nil {
		return "", "", fmt.Errorf("no key is known for %s and the log cannot be read with this token (%v): ask it to send this session any message first, then run it again", host, err)
	}
	var found *msg.Message
	for i := range recs {
		m := recs[i].Msg
		if m == nil || msg.HostOf(m.From) != host || m.Box == "" {
			continue
		}
		if _, valid := m.VerifySig(); !valid {
			continue
		}
		if found == nil || m.At.After(found.At) {
			found = m
		}
	}
	if found == nil {
		return "", "", fmt.Errorf("no key is known for %s: it has not sent a signed message yet (aivoicebus 0.7.0 or later signs every message): ask it to send this session a message, then run it again", host)
	}
	if key, _, ok := config.KnownMachine(host); ok && key != found.Key {
		return "", "", errors.New("the key " + host + " signs with now is not the one pinned here (KEY CHANGED): not sealing to it; check with its owner, then `aivoicebus keys forget " + host + "`")
	}
	_ = config.PinMachine(host, found.Key, found.Box)
	return found.Key, found.Box, nil
}

// BoxKey is the X25519 key to seal for machine host (see MachineKeys).
func BoxKey(c *client.Client, host string) ([]byte, error) {
	_, box, err := MachineKeys(c, host)
	if err != nil {
		return nil, err
	}
	b, err := base64.StdEncoding.DecodeString(box)
	if err != nil || len(b) != 32 {
		return nil, fmt.Errorf("the box key known for %s is not one: ask it to send this session a message, then run it again", host)
	}
	return b, nil
}
