package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/store"
)

// exportCmd writes every record the mesh holds, one JSON object per line, in the
// order a node would apply them. The file is a complete copy of the board: add-only
// records are what replication moves, so it restores a node that lost its disk.
func exportCmd(args []string, s Streams) error {
	fs := newFlagSet("export")
	out := fs.String("out", "", "write to this file (mode 600) instead of standard output")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	c, err := client.FromEnv()
	if err != nil {
		return err
	}
	// The file is written next to its destination and renamed into place only when the export
	// is complete, so a failed export never replaces an earlier good backup.
	w, finish := s.Out, func(bool) error { return nil }
	if *out != "" {
		tmp := *out + ".partial"
		f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		w = f
		finish = func(ok bool) error {
			if err := f.Close(); err != nil || !ok {
				os.Remove(tmp)
				return err
			}
			return os.Rename(tmp, *out)
		}
	}
	bw := bufio.NewWriter(w)
	vv := map[string]uint64{}
	total := 0
	for {
		recs, err := c.SyncAll(vv) // every node, so one that is behind cannot end the export early
		if err != nil {
			finish(false)
			return fmt.Errorf("after %d record(s): %v", total, err)
		}
		progressed := false
		for _, r := range recs {
			if r.Seq != vv[r.Origin]+1 {
				continue // the feed is contiguous per origin; anything else is not ours to write
			}
			b, _ := json.Marshal(r)
			bw.Write(append(b, '\n'))
			vv[r.Origin] = r.Seq
			total++
			progressed = true
		}
		if !progressed {
			break
		}
	}
	if err := bw.Flush(); err != nil {
		finish(false)
		return err
	}
	if err := finish(true); err != nil {
		return err
	}
	fmt.Fprintf(s.Err, "exported %d record(s) from %d origin(s)\n", total, len(vv))
	return nil
}

// importCmd loads an export into a node's data directory. The node that uses the
// directory must be stopped (nothing stops you importing under a running node, and its next
// write would race the import); start it afterwards and it serves what was imported and catches
// up with its peers, which it should be able to reach: a node whose log is older than what its
// peers hold must hear from them before it takes writes, or it would number new records over
// ones they already have. Run as root, the files are given the directory's owner. Records already present are skipped, so running
// it twice, or into a directory that holds some of them, adds only what is missing.
func importCmd(args []string, s Streams) error {
	fs := newFlagSet("import")
	data := fs.String("data", "", "the node's data directory (required; the node must be stopped)")
	node := fs.String("node", "import", "the node's id, as it will serve under")
	if err := parseFlags(fs, args, s); err != nil {
		return err
	}
	if *data == "" || fs.NArg() != 1 {
		return errors.New("usage: import --data DIR [--node NAME] FILE   (FILE from `aivoicebus export`; stop the node first)")
	}
	var in io.Reader
	if fs.Arg(0) == "-" {
		in = s.In
	} else {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}
	st, err := store.Open(*data, *node)
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	before := st.VV()
	var batch []msg.Record
	lines, added, present := 0, 0, 0
	flush := func() error {
		n, err := st.Merge(batch)
		added += n
		batch = batch[:0]
		return err
	}
	for sc.Scan() {
		var r msg.Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil || r.Origin == "" || r.Seq == 0 || (r.Msg == nil) == (r.Ack == nil) {
			return fmt.Errorf("line %d is not a record; is this a aivoicebus export?", lines+1)
		}
		lines++
		if r.Seq <= before[r.Origin] {
			present++
		}
		if batch = append(batch, r); len(batch) == 500 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	matchOwner(*data)
	fmt.Fprintf(s.Out, "read %d record(s): %d added, %d already there", lines, added, present)
	if skipped := lines - added - present; skipped > 0 {
		fmt.Fprintf(s.Out, ", %d out of order or after a gap (not applied)", skipped)
	}
	fmt.Fprintln(s.Out)
	return nil
}
