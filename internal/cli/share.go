package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/identity"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
	"github.com/baditaflorin/aivoicebus/internal/share"
	"github.com/baditaflorin/aivoicebus/internal/view"
)

const shareUsage = "usage: share put ID FILE [--name N] | share put ID --diff [--name N] [-- GIT-DIFF-ARGS] | share put ID FILE --via pipe | " +
	"share get ID NAME [--version N] [--out FILE] | share ls ID | share notes ID [--edit --base N [--file F]] | share add ID HOST"

// shareCmd is a thread's shared space: files, diffs and a notes page its machines keep for days,
// encrypted here for the thread's participants (internal/share).
func shareCmd(args []string, s Streams) error {
	if len(args) == 0 {
		return outbox.Usagef(shareUsage)
	}
	switch args[0] {
	case "put":
		return sharePutCmd(args[1:], s)
	case "get":
		return shareGetCmd(args[1:], s)
	case "ls", "list":
		return shareLsCmd(args[1:], s)
	case "notes":
		return shareNotesCmd(args[1:], s)
	case "add":
		return shareAddCmd(args[1:], s)
	case "-h", "-help", "--help":
		fmt.Fprintln(s.Out, shareUsage)
		return nil
	}
	return outbox.Usagef(shareUsage)
}

// interleaved parses flags written before, between and after the positional words, and returns
// the words. A lone -- ends the flags: what follows is returned after the words, as rest.
func interleaved(fs *flag.FlagSet, args []string, s Streams) (words, rest []string, err error) {
	if i := indexOf(args, "--"); i >= 0 {
		args, rest = args[:i], args[i+1:]
	}
	for {
		if err := parseFlags(fs, args, s); err != nil {
			return nil, nil, err
		}
		if fs.NArg() == 0 {
			return words, rest, nil
		}
		words = append(words, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

// openSpace connects and opens the space of thread id, with an agent reader's screen set.
func openSpace(id string, name string) (*share.Space, error) {
	c, err := client.FromEnv()
	if err != nil {
		return nil, err
	}
	c.Timeout = 2 * time.Minute
	sp, err := share.Open(c, identity.Me(), id)
	if err != nil {
		return nil, err
	}
	if os.Getenv("AIVOICEBUS_AGENT") != "" {
		sp.Screen = share.AgentScreen(sp.Thread, name)
	}
	return sp, nil
}

func printShareNotes(sp *share.Space, w io.Writer) {
	for _, n := range sp.Notes {
		fmt.Fprintln(w, "  "+view.OneLine(n, 200))
	}
}

func sharePutCmd(args []string, s Streams) error {
	fs := newFlagSet("share put")
	name := fs.String("name", "", "the name to share it under (default: the file's base name; changes.diff for --diff)")
	diff := fs.Bool("diff", false, "share the output of `git diff` here (arguments after -- go to git diff)")
	via := fs.String("via", "", "pipe: hand the file over once through the fleet pipe instead (a one-time link in a reply in the thread, read once, gone in 5 minutes)")
	asJSON := fs.Bool("json", false, "print the stored version as JSON")
	words, rest, err := interleaved(fs, args, s)
	if err != nil {
		return err
	}
	if len(words) < 1 || len(words) > 2 || (*diff) == (len(words) == 2) || (len(rest) > 0 && !*diff) {
		return outbox.Usagef("usage: share put ID FILE [--name N] [--via pipe] [--json]   or   share put ID --diff [--name N] [-- GIT-DIFF-ARGS]")
	}
	id := words[0]
	if *via != "" {
		if *via != "pipe" || *diff {
			return outbox.Usagef("--via takes pipe, with a FILE (not --diff): `aivoicebus share put ID FILE --via pipe`")
		}
		return sharePipe(id, words[1], *name, s)
	}
	var data []byte
	kind := share.KindFile
	if *diff {
		kind = share.KindDiff
		if *name == "" {
			*name = "changes.diff"
		}
		cmd := exec.Command("git", append([]string{"diff"}, rest...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if data, err = cmd.Output(); err != nil {
			return fmt.Errorf("git diff failed (%v: %s): run it again inside the repository, with the arguments after -- checked", err, view.OneLine(stderr.String(), 200))
		}
		if len(data) == 0 {
			return errors.New("git diff shows no changes, so there is nothing to share: stage nothing, or name what to compare after -- (`aivoicebus share put ID --diff -- main`)")
		}
	} else {
		path := words[1]
		if *name == "" {
			*name = filepath.Base(path)
		}
		st, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("cannot read %s (%v): check the path and run it again", path, err)
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file: share a file (tar a directory first), then run it again", path)
		}
		if st.Size() > msg.ShareThreadMax {
			return fmt.Errorf("%s is %d MiB, more than a shared space holds (%d MiB): hand it over once with `aivoicebus share put %s %s --via pipe`", path, st.Size()>>20, msg.ShareThreadMax>>20, id, path)
		}
		if data, err = os.ReadFile(path); err != nil {
			return fmt.Errorf("cannot read %s (%v): check the path and run it again", path, err)
		}
	}
	sp, err := openSpace(id, *name)
	if err != nil {
		return err
	}
	rec, err := sp.Put(*name, kind, data, 0)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, map[string]any{"thread": sp.Thread, "name": *name, "version": rec.Ver.Num, "id": rec.Ver.ID, "size": len(data), "participants": sp.Participants(), "notes": sp.Notes})
	}
	fmt.Fprintf(s.Out, "shared %s as version %d in thread %s (%s, sealed for %s)\n", view.OneLine(*name, share.MaxName), rec.Ver.Num, sp.Thread, share.SizeText(int64(len(data))), strings.Join(sp.Participants(), ", "))
	printShareNotes(sp, s.Err)
	fmt.Fprintf(s.Out, "the others read it with: aivoicebus share get %s %s\n", sp.Thread, shellWord(*name))
	return nil
}

// shellWord quotes a name for a command line when it needs it.
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-+@:") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// sharePipe hands a file over once through the fleet pipe: a reply in the thread carrying the
// one-time link, sealed for the machine replied to (as `reply --file`).
func sharePipe(id, path, name string, s Streams) error {
	text := "shared once through the pipe: " + filepath.Base(path)
	if name != "" {
		text = "shared once through the pipe: " + name
	}
	args := []string{"--re", id, "--file", path, "--via", "pipe"}
	if name != "" {
		args = append(args, "--meta", "name="+name)
	}
	return sendWith(append(args, "--", text), s, true)
}

func shareGetCmd(args []string, s Streams) error {
	fs := newFlagSet("share get")
	ver := fs.Int("version", 0, "which version (default: the latest)")
	vid := fs.String("id", "", "a version by its id (or its first characters): one written on two nodes at once, which ls shows beside the other")
	out := fs.String("out", "", "save it to this file instead of printing it")
	force := fs.Bool("force", false, "with --out: replace the file if it exists")
	asJSON := fs.Bool("json", false, "print the version and its text (or base64) as JSON")
	words, _, err := interleaved(fs, args, s)
	if err != nil {
		return err
	}
	if len(words) != 2 || *ver < 0 {
		return outbox.Usagef("usage: share get ID NAME [--version N] [--out FILE] [--force] [--json]")
	}
	return shareRead(words[0], words[1], share.KindFile, *ver, *vid, *out, *force, *asJSON, s)
}

// shareRead prints or saves one version, with a line about it on stderr.
func shareRead(id, name, kind string, ver int, vid, out string, force, asJSON bool, s Streams) error {
	sp, err := openSpace(id, name)
	if err != nil {
		return err
	}
	v, data, err := sp.Get(name, kind, ver, vid)
	printShareNotes(sp, s.Err)
	if err != nil {
		return err
	}
	if asJSON {
		r := map[string]any{"thread": sp.Thread, "name": v.Name, "kind": v.Kind, "version": v.Num, "id": v.ID, "by": v.By, "at": v.At, "size": v.Size, "sha256": v.SHA256, "verified": v.Verified}
		if utf8.Valid(data) {
			r["text"] = string(data)
		} else {
			r["base64"] = data
		}
		return writeJSON(s.Out, r)
	}
	fmt.Fprintf(s.Err, "%s version %d of thread %s, by %s %s, %s%s\n", view.OneLine(v.Name, share.MaxName), v.Num, sp.Thread, view.Name(v.By, msg.MaxIdentity),
		view.When(v.At, time.Now()), share.SizeText(v.Size), verdictNote(v.Verified))
	if out != "" {
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(out, flags, 0o600)
		if err != nil {
			return fmt.Errorf("cannot write %s (%v): run it again with another `--out`, or with `--force` to replace it", out, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fmt.Fprintf(s.Out, "saved to %s\n", out)
		return nil
	}
	if !utf8.Valid(data) && isTerminalWriter(s.Out) {
		return fmt.Errorf("%s is not text, so it is not printed to the terminal: save it with `--out FILE`", name)
	}
	_, err = s.Out.Write(data)
	return err
}

func isTerminalWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0 && f.Name() != os.DevNull
}

func verdictNote(v string) string {
	switch v {
	case msg.VerifiedSigned:
		return ", signed"
	case msg.VerifiedChanged:
		return ", SIGNED BY ANOTHER KEY than the one pinned for that machine"
	case msg.VerifiedBad:
		return ", SIGNATURE DOES NOT VERIFY"
	}
	return ""
}

func shareLsCmd(args []string, s Streams) error {
	fs := newFlagSet("share ls")
	asJSON := fs.Bool("json", false, "print the space as JSON")
	words, _, err := interleaved(fs, args, s)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return outbox.Usagef("usage: share ls ID [--json]")
	}
	sp, err := openSpace(words[0], "")
	if err != nil {
		return err
	}
	l, err := sp.List()
	printShareNotes(sp, s.Err)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(s.Out, l)
	}
	share.Describe(s.Out, l, time.Now())
	fmt.Fprintf(s.Out, "participants: %s\n", strings.Join(l.Participants, ", "))
	if !l.HasKey {
		fmt.Fprintf(s.Out, "this machine does not hold the space's key yet: a participant's next `aivoicebus share ls %s` seals it for it\n", l.Thread)
	}
	now := time.Now()
	for _, e := range l.Entries {
		v := e.Latest
		extra := ""
		if n := len(e.Versions); n > 1 {
			extra = fmt.Sprintf(", %d versions", n)
		}
		if e.Conflict {
			extra += ", CONFLICT: two versions of one number were written on two nodes at once (`share get --id` reads the other)"
		}
		label := view.OneLine(e.Name, share.MaxName)
		if e.Kind == share.KindNotes {
			label = "[notes]"
		}
		fmt.Fprintf(s.Out, "  %-24s v%-3d %9s  %s %s%s%s\n", label, v.Num, share.SizeText(v.Size), view.Name(v.By, msg.MaxIdentity), view.When(v.At, now), verdictNote(v.Verified), extra)
	}
	if l.Unreadable > 0 {
		fmt.Fprintf(s.Out, "  (%d version(s) this machine cannot open: sealed with a key it lacks, or not written with the thread's key)\n", l.Unreadable)
	}
	if len(l.Entries) == 0 {
		fmt.Fprintf(s.Out, "  (empty: `aivoicebus share put %s FILE` adds a file, `aivoicebus share notes %s --edit --base 0` starts the notes)\n", l.Thread, l.Thread)
	}
	return nil
}

func shareNotesCmd(args []string, s Streams) error {
	fs := newFlagSet("share notes")
	edit := fs.Bool("edit", false, "write a new version of the notes: from --file, from stdin, or in $EDITOR at a terminal")
	base := fs.Int("base", -1, "with --edit: the version you edited (0 for the first page); refused if someone wrote a newer one")
	file := fs.String("file", "", "with --edit: the new text, from this file (- for stdin)")
	ver := fs.Int("version", 0, "read this version (default: the latest)")
	asJSON := fs.Bool("json", false, "print as JSON")
	words, _, err := interleaved(fs, args, s)
	if err != nil {
		return err
	}
	if len(words) != 1 {
		return outbox.Usagef("usage: share notes ID [--version N] [--json]   or   share notes ID --edit --base N [--file F]")
	}
	id := words[0]
	if !*edit {
		if *base >= 0 || *file != "" {
			return outbox.Usagef("--base and --file go with --edit: `aivoicebus share notes ID --edit --base N --file F`")
		}
		return shareRead(id, share.NotesName, share.KindNotes, *ver, "", "", false, *asJSON, s)
	}
	var text []byte
	switch {
	case *file != "" && *file != "-":
		if text, err = os.ReadFile(*file); err != nil {
			return fmt.Errorf("cannot read %s (%v): check the path and run it again", *file, err)
		}
	case *file == "-" || !isTerminal(s.In):
		if text, err = io.ReadAll(io.LimitReader(s.In, msg.ShareChunk+1)); err != nil {
			return err
		}
	default: // a person at a terminal: edit the latest in $EDITOR, which names the base itself
		return shareNotesEditor(id, *base, s)
	}
	if *base < 0 {
		return outbox.Usagef("name the version you edited: --base N (`aivoicebus share notes %s` shows it; 0 for the first page)", id)
	}
	if len(text) > msg.ShareChunk {
		return fmt.Errorf("the notes are limited to %d MiB: share a longer text as a file with `aivoicebus share put %s FILE`", msg.ShareChunk>>20, id)
	}
	return shareNotesPut(id, text, *base, s)
}

func shareNotesPut(id string, text []byte, base int, s Streams) error {
	sp, err := openSpace(id, share.NotesName)
	if err != nil {
		return err
	}
	rec, err := sp.Put(share.NotesName, share.KindNotes, text, base)
	printShareNotes(sp, s.Err)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "the notes of thread %s are now version %d (sealed for %s)\n", sp.Thread, rec.Ver.Num, strings.Join(sp.Participants(), ", "))
	return nil
}

// shareNotesEditor opens the latest notes in $EDITOR and writes them back as the next version.
func shareNotesEditor(id string, base int, s Streams) error {
	sp, err := openSpace(id, share.NotesName)
	if err != nil {
		return err
	}
	cur, data := 0, []byte{}
	if v, d, err := sp.Get(share.NotesName, share.KindNotes, 0, ""); err == nil {
		cur, data = v.Num, d
	} else if l, lerr := sp.List(); lerr != nil || hasNotes(l) {
		return err
	}
	if base >= 0 && base != cur {
		return outbox.Usagef("at a terminal --edit opens the latest notes (version %d) in $EDITOR: leave --base out, or give the text with --file", cur)
	}
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	f, err := os.CreateTemp("", "aivoicebus-notes-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	f.Close()
	cmd := exec.Command("sh", "-c", editor+` "$1"`, "editor", f.Name())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the editor (%s) failed: %v; set $EDITOR and try again, or give the text with `--file`", editor, err)
	}
	text, err := os.ReadFile(f.Name())
	if err != nil {
		return err
	}
	if bytes.Equal(text, data) {
		fmt.Fprintln(s.Out, "no change: nothing was stored")
		return nil
	}
	return shareNotesPut(id, text, cur, s)
}

func hasNotes(l share.Listing) bool {
	for _, e := range l.Entries {
		if e.Kind == share.KindNotes {
			return true
		}
	}
	return false
}

func shareAddCmd(args []string, s Streams) error {
	fs := newFlagSet("share add")
	words, _, err := interleaved(fs, args, s)
	if err != nil {
		return err
	}
	if len(words) != 2 {
		return outbox.Usagef("usage: share add ID HOST   (seal the space's key for one more machine)")
	}
	sp, err := openSpace(words[0], "")
	if err != nil {
		return err
	}
	err = sp.Add(words[1])
	printShareNotes(sp, s.Err)
	if err != nil {
		return err
	}
	fmt.Fprintf(s.Out, "%s can read and write thread %s's shared space (participants: %s)\n", strings.ToLower(words[1]), sp.Thread, strings.Join(sp.Participants(), ", "))
	return nil
}
