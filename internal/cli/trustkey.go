package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/baditaflorin/aivoicebus/internal/release"
)

// trustKeyCmd manages the keys this machine accepts signatures from, besides the ones built into the binary.
func trustKeyCmd(args []string, s Streams) error {
	if len(args) == 0 || wantsHelp(args) {
		fmt.Fprintln(s.Out, `usage:
  aivoicebus trust-key list
  aivoicebus trust-key add <base64-public-key> [--label TEXT] [--fingerprint SHA256:...]
  aivoicebus trust-key rm <fingerprint>

A trusted key can sign the releases and node lists this machine accepts: whoever holds its private
half can make this machine install and run their binary. Add a key only for a mesh you run
yourself, and only after checking its fingerprint with the person who made it, by another route
than the one you got the key from. The official project key is built into the binary.`)
		if len(args) == 0 {
			return exitError{2}
		}
		return nil
	}
	switch args[0] {
	case "list":
		return trustKeyList(args[1:], s)
	case "add":
		return trustKeyAdd(args[1:], s)
	case "rm", "remove":
		return trustKeyRemove(args[1:], s)
	}
	return fmt.Errorf("unknown trust-key command %q (list, add or rm)", args[0])
}

func trustKeyList(args []string, s Streams) error {
	fs := newFlagSet("trust-key list")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	keys, notes := release.Keys()
	for _, k := range keys {
		label := k.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(s.Out, "%s  %-22s %s\n", release.Fingerprint(k.Public), k.Source, label)
	}
	for _, n := range notes {
		fmt.Fprintln(s.Err, "warning:", n)
	}
	return nil
}

func trustKeyAdd(args []string, s Streams) error {
	fs := newFlagSet("trust-key add")
	label := fs.String("label", "", "a note about whose key this is")
	want := fs.String("fingerprint", "", "the fingerprint you were given for this key (SHA256:...); the add is refused if it differs")
	// the key comes first on the command line; flags after it
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: aivoicebus trust-key add <base64-public-key> [--label TEXT] [--fingerprint SHA256:...]")
	}
	keyText := args[0]
	if err := parseFlags(fs, args[1:], s); err != nil {
		return err
	}
	k, err := release.ParseKey(keyText)
	if err != nil {
		return err
	}
	fp := release.Fingerprint(k)
	fmt.Fprintf(s.Out, "key:         %s\nfingerprint: %s\n", release.EncodeKey(k), fp)
	fmt.Fprintln(s.Out, "Anyone holding this key's private half can make this machine install and run their binary.")
	switch {
	case *want != "":
		if *want != fp {
			return fmt.Errorf("the fingerprint does not match: you expected %s, this key is %s; nothing was added", *want, fp)
		}
	default:
		got, err := askLine(s, "Type the fingerprint to trust this key: ")
		switch {
		case errors.Is(err, errNoTerminal):
			return errors.New("adding a trusted key needs a terminal to type its fingerprint on, or --fingerprint with the one you were given")
		case err != nil:
			return err
		case got != fp:
			return errors.New("that is not the fingerprint: nothing was added")
		}
	}
	added, err := release.AddUserKey(k, *label)
	if err != nil {
		return err
	}
	if !added {
		fmt.Fprintln(s.Out, "already trusted: nothing changed")
		return nil
	}
	fmt.Fprintf(s.Out, "added to %s\n", release.UserKeyFile())
	return nil
}

func trustKeyRemove(args []string, s Streams) error {
	fs := newFlagSet("trust-key rm")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: aivoicebus trust-key rm <fingerprint>  (see `trust-key list`; built-in keys cannot be removed)")
	}
	ok, err := release.RemoveUserKey(fs.Arg(0))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no key you added has the fingerprint %s (built-in keys cannot be removed)", fs.Arg(0))
	}
	fmt.Fprintln(s.Out, "removed")
	return nil
}
