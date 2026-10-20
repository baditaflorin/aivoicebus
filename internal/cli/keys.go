package cli

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/share"
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
			return outbox.NotFoundf("no key is pinned for %s: `aivoicebus keys` lists the pinned ones", args[1])
		}
		fmt.Fprintf(s.Out, "forgot %s's key: the next one seen is pinned. Check with that machine's owner that it really changed.\n", args[1])
		return nil
	}
	return usage
}

// boxKeyFor is the X25519 key messages to host are sealed to (share.BoxKey: the pinned one, else
// the one in the most recent message host signed).
func boxKeyFor(c *client.Client, host string) ([]byte, error) { return share.BoxKey(c, host) }
