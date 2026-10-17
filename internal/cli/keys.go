package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// fingerprint is a short, comparable form of a base64 public key.
func fingerprint(b64 string) string {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])[:16]
}

// keysCmd shows this machine's key, the keys pinned for other machines, and forgets one so its
// new key is pinned (after a machine was reinstalled, say: check with its owner first).
func keysCmd(args []string, s Streams) error {
	usage := outbox.Usagef("usage: keys [list]   |   keys forget HOST")
	if len(args) == 0 || args[0] == "list" {
		priv, box, err := config.MachineKeys()
		if err != nil {
			return err
		}
		pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
		fmt.Fprintf(s.Out, "this machine signs with %s (box %s)\n", fingerprint(pub), fingerprint(base64.StdEncoding.EncodeToString(msg.BoxPublic(box))))
		fmt.Fprintln(s.Out, "pinned for other machines (the first key seen; a different one shows as KEY CHANGED):")
		n := 0
		for _, line := range strings.Split(config.KnownMachinesText(), "\n") {
			fs := strings.Fields(line)
			if len(fs) < 2 {
				continue
			}
			n++
			box := "no box key yet"
			if len(fs) >= 3 {
				box = "box " + fingerprint(fs[2])
			}
			fmt.Fprintf(s.Out, "  %-32s %s (%s)\n", fs[0], fingerprint(fs[1]), box)
		}
		if n == 0 {
			fmt.Fprintln(s.Out, "  (none yet: a machine's key is pinned when one of its signed messages is first read here)")
		}
		return nil
	}
	if args[0] == "forget" && len(args) == 2 {
		ok, err := config.ForgetMachine(args[1])
		if err != nil {
			return err
		}
		if !ok {
			return outbox.NotFoundf("no key is pinned for %s", args[1])
		}
		fmt.Fprintf(s.Out, "forgot %s's key: the next one seen is pinned. Check with that machine's owner that it really changed.\n", args[1])
		return nil
	}
	return usage
}

// boxKeyFor is the X25519 key messages to host are sealed to: the pinned one, else the one in the
// most recent message host signed (read from the log, which needs the shared or observer token).
func boxKeyFor(c *client.Client, host string) ([]byte, error) {
	if _, box, ok := config.KnownMachine(host); ok && box != "" {
		return base64.StdEncoding.DecodeString(box)
	}
	recs, err := c.SyncAll(map[string]uint64{})
	if err != nil {
		return nil, fmt.Errorf("no key is known for %s and the log cannot be read with this token (%v): it must send any message to this session first", host, err)
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
		return nil, fmt.Errorf("no key is known for %s: it has not sent a signed message yet (aivoicebus 0.7.0 or later signs every message)", host)
	}
	if key, _, ok := config.KnownMachine(host); ok && key != found.Key {
		return nil, errors.New("the key " + host + " signs with now is not the one pinned here (KEY CHANGED): not sealing to it; check with its owner, then `aivoicebus keys forget " + host + "`")
	}
	_ = config.PinMachine(host, found.Key, found.Box)
	return base64.StdEncoding.DecodeString(found.Box)
}
