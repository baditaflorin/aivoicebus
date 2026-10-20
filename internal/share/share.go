// Package share is a session's side of a thread's shared space (msg/share.go): files, diffs and a
// notes page the thread's machines keep for days. It encrypts on this machine, seals the thread's
// key for each participant machine, signs what it writes, and judges what it reads: a node holds
// only ciphertext, and a version whose signature fails, or whose text reads like instructions to
// an agent, is held back from an agent until the person releases it.
//
// Who is in a thread: the machine that started it and the one it was sent to, then every machine
// a participant wrote to in it (a signed message for one machine), and every machine a participant
// sealed the key for (`aivoicebus share add`). A broadcast adds no one. Key records are trusted only
// when a participant's machine key signed them, so a machine outside the thread can neither read
// the space nor slip in a key of its own.
package share

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/baditaflorin/aivoicebus/internal/client"
	"github.com/baditaflorin/aivoicebus/internal/config"
	"github.com/baditaflorin/aivoicebus/internal/msg"
	"github.com/baditaflorin/aivoicebus/internal/outbox"
)

// NotesName is the notes page's name.
const NotesName = "notes"

// MaxName is the longest name a file may be shared under.
const MaxName = 120

// ErrOldNode is a node that holds no shared spaces.
type ErrOldNode struct{ Node, Version string }

func (e *ErrOldNode) Error() string {
	v := e.Version
	if v == "" {
		v = "an older release"
	}
	return fmt.Sprintf("the node %s runs %s, which holds no shared spaces (%s or later does): ask the user to update the nodes (`aivoicebus nodes` lists them), or hand a file over once with `aivoicebus share put ID FILE --via pipe`", e.Node, v, msg.ShareSince)
}

// HeldError is a version held back from an agent.
type HeldError struct{ ID, Why string }

func (e *HeldError) Error() string {
	return fmt.Sprintf("[held for the user: %s] the person can read it with `aivoicebus held` and let agents see it with `aivoicebus unhold %s`; do not try to fetch it another way", e.Why, e.ID)
}

// ConflictError is a write refused because someone wrote a newer version first.
type ConflictError struct {
	Thread, Name   string
	Latest, Edited int
	Notes          bool
}

func (e *ConflictError) Error() string {
	if e.Notes {
		return fmt.Sprintf("refused: the notes are at version %d and you edited version %d, so nothing was stored: get them (`aivoicebus share notes %s`), merge your change in, and put again with `--base %d`", e.Latest, e.Edited, e.Thread, e.Latest)
	}
	return fmt.Sprintf("refused: %s is at version %d and you edited version %d, so nothing was stored: get it (`aivoicebus share get %s %s`), merge your change in, and put again", e.Name, e.Latest, e.Edited, e.Thread, e.Name)
}

// Space is one thread's shared space as this session sees it.
type Space struct {
	C      *client.Client
	Me     string
	Thread string // the thread's root message id
	// Screen, when set, judges a version another machine wrote before its bytes are returned: a
	// reason means it is held back (see HeldError). The commands set it for an agent reader.
	Screen func(id string, m msg.Message) string
	// Notes says what was done along the way (a key sealed for a new participant), for the caller.
	Notes []string

	msgs    []msg.Message
	idx     msg.ShareIndex
	parts   map[string]bool   // participant machines
	signKey map[string]string // machine -> its ed25519 key (base64) as its signed messages carry it
	boxKey  map[string]string // machine -> its box key (base64)
	keys    map[string][]byte // key id -> K, the keys this machine opened
	keyAt   map[string]time.Time
	sign    ed25519.PrivateKey
	box     []byte
	host    string
	looked  map[string]bool // machines whose keys were looked for in the log
}

// Open loads thread id's space (id may be any message of the thread). It refuses on a node that
// holds no shared spaces.
func Open(c *client.Client, me, id string) (*Space, error) {
	if !msg.ValidID(id) {
		return nil, fmt.Errorf("%q is not a message id: name the thread by a message's full 10-character id (`aivoicebus inbox --all` shows them)", id)
	}
	h, err := c.Health()
	if err != nil {
		return nil, err
	}
	if !h.Has(msg.FeatureShare) {
		return nil, &ErrOldNode{Node: h.Node, Version: h.Version}
	}
	s := &Space{C: c, Me: me, host: msg.HostOf(me)}
	if s.sign, s.box, err = config.MachineKeys(); err != nil {
		return nil, fmt.Errorf("this machine's keys cannot be read (%v): run `aivoicebus doctor`", err)
	}
	ms, err := c.Thread(strings.ToLower(id))
	if err != nil {
		return nil, err
	}
	for _, m := range ms {
		if m.ID == strings.ToLower(id) {
			s.Thread = m.Thread
		}
	}
	if s.Thread == "" {
		return nil, fmt.Errorf("no node holds message %s: check the id with `aivoicebus inbox --all`", id)
	}
	if s.Thread != strings.ToLower(id) { // the thread's own messages, not the reply's
		if ms, err = c.Thread(s.Thread); err != nil {
			return nil, err
		}
	}
	s.msgs = ms
	return s, s.reload()
}

// reload reads the index again and works out who is in, and which keys this machine holds.
func (s *Space) reload() error {
	idx, err := s.C.ShareIndex(s.Thread)
	if client.IsNotFound(err) {
		return &ErrOldNode{Node: "answering", Version: s.C.Server.Version}
	}
	if err != nil {
		return err
	}
	s.idx = idx
	s.machines()
	s.openKeys()
	return nil
}

// singleMachine is the one machine a target names, or "" for a broadcast, a selector or a role.
func singleMachine(to string) string {
	if to == "" || to == "all" || strings.HasPrefix(to, "@") || strings.HasPrefix(to, "*") || msg.IsSelector(to) {
		return ""
	}
	return msg.HostOf(to)
}

// machines works out the participants and their keys.
func (s *Space) machines() {
	s.parts, s.signKey, s.boxKey = map[string]bool{}, map[string]string{}, map[string]string{}
	var root *msg.Message
	for i := range s.msgs {
		m := s.msgs[i]
		if m.Verified != msg.VerifiedSigned {
			continue
		}
		h := msg.HostOf(m.From)
		s.signKey[h] = m.Key // the newest signed message wins (messages come oldest first)
		if m.Box != "" {
			s.boxKey[h] = m.Box
		}
		if m.ID == s.Thread {
			root = &s.msgs[i]
		}
	}
	if root != nil {
		s.parts[msg.HostOf(root.From)] = true
		if h := singleMachine(root.To); h != "" {
			s.parts[h] = true
		}
	} else if s.holdsKeyFrom() {
		s.parts[s.host] = true // the first message is gone: a machine let in before is still in
	}
	for changed := true; changed; {
		changed = false
		for _, m := range s.msgs {
			if m.Verified != msg.VerifiedSigned || !s.parts[msg.HostOf(m.From)] {
				continue
			}
			if h := singleMachine(m.To); h != "" && !s.parts[h] {
				s.parts[h], changed = true, true
			}
		}
		for _, r := range s.idx.Records {
			if k := r.Key; k != nil && s.trustedKey(*k) && !s.parts[k.For] {
				s.parts[k.For], changed = true, true
			}
		}
	}
}

// holdsKeyFrom reports whether a key record for this machine opens with its key.
func (s *Space) holdsKeyFrom() bool {
	for _, r := range s.idx.Records {
		if k := r.Key; k != nil && k.For == s.host {
			if _, err := openKey(*k, s.Thread, s.box); err == nil {
				return true
			}
		}
	}
	return false
}

// signKeyOf is the ed25519 key machine host signs with: from its signed messages in the thread,
// else the one pinned here.
func (s *Space) signKeyOf(host string) string {
	if k := s.signKey[host]; k != "" {
		return k
	}
	if host == s.host {
		return base64.StdEncoding.EncodeToString(s.sign.Public().(ed25519.PublicKey))
	}
	if k, _, ok := config.KnownMachine(host); ok {
		return k
	}
	if s.looked == nil {
		s.looked = map[string]bool{}
	}
	if !s.looked[host] && s.parts[host] { // a participant that has not written in the thread: from the log, once
		s.looked[host] = true
		if k, b, err := MachineKeys(s.C, host); err == nil {
			s.signKey[host], s.boxKey[host] = k, b
			return k
		}
	}
	return ""
}

func verifySig(key, sig string, signed []byte) bool {
	k, err1 := base64.StdEncoding.DecodeString(key)
	g, err2 := base64.StdEncoding.DecodeString(sig)
	return err1 == nil && err2 == nil && len(k) == ed25519.PublicKeySize && len(g) == ed25519.SignatureSize && ed25519.Verify(ed25519.PublicKey(k), signed, g)
}

// trustedKey reports whether a participant's machine sealed and signed key record k. A
// participant whose signing key is not known yet (it has not written in the thread, and nothing
// here pinned it) is trusted on first use and pinned, as a machine's first signed message is.
func (s *Space) trustedKey(k msg.ShareKey) bool {
	by := msg.HostOf(k.By)
	if !s.parts[by] || !verifySig(k.SigKey, k.Sig, k.SignedBytes(s.Thread)) {
		return false
	}
	want := s.signKeyOf(by)
	if want == "" {
		if config.PinMachine(by, k.SigKey, "") == nil {
			s.signKey[by] = k.SigKey
		}
		return true
	}
	return k.SigKey == want
}

// openKeys opens every trusted key record sealed for this machine.
func (s *Space) openKeys() {
	s.keys, s.keyAt = map[string][]byte{}, map[string]time.Time{}
	for _, r := range s.idx.Records {
		k := r.Key
		if k == nil || k.For != s.host || !s.trustedKey(*k) {
			continue
		}
		if _, have := s.keys[k.ID]; have {
			continue
		}
		if key, err := openKey(*k, s.Thread, s.box); err == nil {
			s.keys[k.ID], s.keyAt[k.ID] = key, r.At
		}
	}
}

// Participants are the machines in the thread, sorted.
func (s *Space) Participants() []string {
	var out []string
	for h := range s.parts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// Has reports whether this machine holds the space's key.
func (s *Space) Has() bool { return len(s.keys) > 0 }

// writeKey is the key new versions use: the oldest this machine holds, so every participant
// settles on the same one when two were made at once.
func (s *Space) writeKey() (string, []byte) {
	best := ""
	for id, at := range s.keyAt {
		if best == "" || at.Before(s.keyAt[best]) || (at.Equal(s.keyAt[best]) && id < best) {
			best = id
		}
	}
	return best, s.keys[best]
}

func (s *Space) notInThread() error {
	return fmt.Errorf("this machine (%s) is not part of thread %s, so it cannot use its shared space: ask a participant (%s) to add it with `aivoicebus share add %s %s`", s.host, s.Thread, strings.Join(s.Participants(), ", "), s.Thread, s.host)
}

func (s *Space) noKey() error {
	return fmt.Errorf("the space's key is not sealed for this machine (%s) yet: if it has not written in the thread, reply in it first (`aivoicebus reply --re %s \"...\"`) so its key is known; then ask a participant (%s) to run `aivoicebus share ls %s`, which seals the key for every machine in the thread, and try again", s.host, s.Thread, strings.Join(s.Participants(), ", "), s.Thread)
}

// ensureKey makes the space's first key when there is none, and seals what this machine holds for
// every participant that lacks it.
func (s *Space) ensureKey() error {
	if !s.parts[s.host] {
		return s.notInThread()
	}
	if !s.Has() {
		for _, r := range s.idx.Records {
			if r.Key != nil && s.trustedKey(*r.Key) {
				return s.noKey() // there is a key, sealed for others only
			}
		}
		k, err := newKey()
		if err != nil {
			return err
		}
		if err := s.putKey(k, s.host, s.box, true); err != nil {
			return err
		}
		if err := s.reload(); err != nil {
			return err
		}
		if !s.Has() {
			return errors.New("the key just made for this space did not come back from the node: try again in a minute")
		}
	}
	return s.Reseal()
}

// putKey seals k for host and stores the record. self is this machine's own box private key
// (sealing for itself), else box is the public key.
func (s *Space) putKey(k []byte, host string, box []byte, self bool) error {
	pub := box
	if self {
		pub = msg.BoxPublic(box)
	}
	sealed, err := sealKeyFor(k, s.Thread, host, pub)
	if err != nil {
		return err
	}
	rec := msg.ShareKey{ID: keyID(k), For: host, Box: base64.StdEncoding.EncodeToString(pub), Sealed: sealed, By: s.Me,
		SigKey: base64.StdEncoding.EncodeToString(s.sign.Public().(ed25519.PublicKey))}
	rec.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(s.sign, rec.SignedBytes(s.Thread)))
	_, err = s.C.ShareKey(s.Thread, rec)
	return err
}

// Reseal seals every key this machine holds for every participant that has no trusted record of
// it, and says so in Notes. A participant whose box key is not known yet is noted and skipped.
func (s *Space) Reseal() error {
	if !s.Has() {
		return nil
	}
	sealed := map[string]bool{} // key id + machine
	for _, r := range s.idx.Records {
		if k := r.Key; k != nil && s.trustedKey(*k) {
			sealed[k.ID+" "+k.For] = true
		}
	}
	did := false
	for _, h := range s.Participants() {
		for id, k := range s.keys {
			if sealed[id+" "+h] {
				continue
			}
			box, err := s.boxOf(h)
			if err != nil {
				s.Notes = append(s.Notes, fmt.Sprintf("cannot seal the space's key for %s yet: %v", h, err))
				break
			}
			if err := s.putKey(k, h, box, false); err != nil {
				return err
			}
			did = true
			s.Notes = append(s.Notes, "sealed the space's key for "+h)
		}
	}
	if did {
		return s.reload()
	}
	return nil
}

// boxOf is machine host's box key: from its signed messages in the thread, else BoxKey.
func (s *Space) boxOf(host string) ([]byte, error) {
	if host == s.host {
		return msg.BoxPublic(s.box), nil
	}
	if b := s.boxKey[host]; b != "" {
		return base64.StdEncoding.DecodeString(b)
	}
	for _, r := range s.idx.Records { // the key it sealed for itself names its box key, signed
		if k := r.Key; k != nil && k.For == host && msg.HostOf(k.By) == host && s.trustedKey(*k) {
			return base64.StdEncoding.DecodeString(k.Box)
		}
	}
	return BoxKey(s.C, host)
}

// Add lets machine host into the space: its key is sealed for it, and it counts as a participant.
func (s *Space) Add(host string) error {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || msg.HostOf(host) != host || len(host) > 64 {
		return fmt.Errorf("%q is not a machine name: name the machine as `aivoicebus who` shows it, before the slash", host)
	}
	if err := s.ensureKey(); err != nil {
		return err
	}
	if s.parts[host] {
		return nil
	}
	box, err := s.boxOf(host)
	if err != nil {
		return err
	}
	for _, k := range s.keys {
		if err := s.putKey(k, host, box, false); err != nil {
			return err
		}
	}
	s.Notes = append(s.Notes, "sealed the space's key for "+host)
	return s.reload()
}

// ValidName checks a name a file is shared under.
func ValidName(name string) error {
	switch {
	case name == "" || len(name) > MaxName || !utf8.ValidString(name):
		return fmt.Errorf("a shared name is 1 to %d characters of text: pick another with `--name`", MaxName)
	case strings.ContainsAny(name, "/\\\x00") || strings.TrimSpace(name) != name || name == "." || name == "..":
		return errors.New("a shared name has no slash, no leading or trailing space and is not . or ..: pick another with `--name`")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("a shared name has no control characters: pick another with `--name`")
		}
	}
	return nil
}

// Put stores data as a new version of name (kind file, diff or notes). A base above 0 names the
// version this one edits, and the node refuses it unless that is still the latest; notes always
// name it (0 for the first page). Content holding a credential is refused before anything leaves.
func (s *Space) Put(name, kind string, data []byte, base int) (msg.ShareRecord, error) {
	if err := ValidName(name); err != nil {
		return msg.ShareRecord{}, err
	}
	if err := checkContent(data); err != nil {
		return msg.ShareRecord{}, err
	}
	if err := outbox.CheckContent([]byte(name)); err != nil {
		return msg.ShareRecord{}, err
	}
	if int64(len(data)) > s.idx.Max && s.idx.Max > 0 {
		return msg.ShareRecord{}, fmt.Errorf("%s is larger than a thread's shared space holds (%d MiB): send it once through the pipe (`aivoicebus share put %s FILE --via pipe`)", name, s.idx.Max>>20, s.Thread)
	}
	if err := s.ensureKey(); err != nil {
		return msg.ShareRecord{}, err
	}
	kid, k := s.writeKey()
	vid := newVersionID()
	sum := sha256.Sum256(data)
	m := meta{Name: name, Kind: kind, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), At: time.Now().UTC()}
	sealedMeta, err := sealMeta(k, s.Thread, vid, m)
	if err != nil {
		return msg.ShareRecord{}, err
	}
	n := max(1, (len(data)+msg.ShareChunk-1)/msg.ShareChunk)
	chunks := make([][]byte, n)
	for i := range n {
		end := min(len(data), (i+1)*msg.ShareChunk)
		if chunks[i], err = sealChunk(k, s.Thread, vid, i, n, data[i*msg.ShareChunk:end]); err != nil {
			return msg.ShareRecord{}, err
		}
	}
	v := msg.ShareVersion{ID: vid, Name: nameID(k, kind, name), Base: base, Notes: kind == KindNotes, KeyID: kid, Meta: sealedMeta, By: s.Me,
		SigKey: base64.StdEncoding.EncodeToString(s.sign.Public().(ed25519.PublicKey))}
	for _, c := range chunks {
		h := sha256.Sum256(c)
		v.Chunks = append(v.Chunks, hex.EncodeToString(h[:]))
	}
	v.Sig = base64.StdEncoding.EncodeToString(ed25519.Sign(s.sign, v.SignedBytes(s.Thread)))
	rec, err := s.upload(v, chunks)
	var se *client.StatusError
	if errors.As(err, &se) && se.Refused() == msg.RefusedConflict {
		latest := 0
		if m := reAtVersion.FindStringSubmatch(se.Text); m != nil {
			latest, _ = strconv.Atoi(m[1])
		}
		return msg.ShareRecord{}, &ConflictError{Thread: s.Thread, Name: name, Latest: latest, Edited: base, Notes: kind == KindNotes}
	}
	return rec, err
}

var reAtVersion = regexp.MustCompile(`at version (\d+)`)

// checkContent is outbox.CheckContent (the send-side secret scan) over data of any size: windows
// of a MiB, each overlapping the next by more than any credential is long, scanned in parallel
// (the scan takes about a second per MiB on one core).
func checkContent(data []byte) error {
	const win, overlap = 1 << 20, 8 << 10
	if len(data) <= win+overlap {
		return outbox.CheckContent(data)
	}
	var parts [][]byte
	for i := 0; i < len(data); i += win {
		parts = append(parts, data[i:min(len(data), i+win+overlap)])
	}
	errs := make([]error, len(parts))
	sem := make(chan struct{}, max(1, runtime.NumCPU()))
	var wg sync.WaitGroup
	for i, p := range parts {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			errs[i] = outbox.CheckContent(p)
			<-sem
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// upload sends the chunks and the version to one node: the first that takes them. A node that
// lost part of the upload (a restart) gets it again once.
func (s *Space) upload(v msg.ShareVersion, chunks [][]byte) (msg.ShareRecord, error) {
	var last error
	for _, u := range s.C.URLs {
		one := &client.Client{URLs: []string{u}, Token: s.C.Token, Timeout: 2 * time.Minute, Deadline: s.C.Deadline}
		for try := 0; try < 2; try++ {
			rec, err := uploadTo(one, s.Thread, v, chunks)
			var se *client.StatusError
			switch {
			case err == nil:
				return rec, nil
			case errors.As(err, &se) && se.Refused() == msg.RefusedShareMissing && try == 0:
				continue
			case client.IsUnreachable(err) || !errors.As(err, &se):
				last = err
			default:
				return rec, err
			}
			break
		}
	}
	if last == nil {
		last = errors.New("no node configured: run `aivoicebus join`, or ask the user for the node URLs")
	}
	return msg.ShareRecord{}, last
}

func uploadTo(c *client.Client, thread string, v msg.ShareVersion, chunks [][]byte) (msg.ShareRecord, error) {
	for i, ch := range chunks {
		h, err := c.ShareUpload(thread, ch)
		if err != nil {
			return msg.ShareRecord{}, err
		}
		if h != v.Chunks[i] {
			return msg.ShareRecord{}, errors.New("the node named a chunk by another hash than this machine computed: try again, and if it persists ask the user to check the node (`aivoicebus nodes`)")
		}
	}
	return c.SharePut(thread, v)
}

// Version is one version as this machine reads it.
type Version struct {
	Num      int       `json:"version"`
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256"`
	By       string    `json:"by"`
	At       time.Time `json:"at"` // when the node took it
	Base     int       `json:"base,omitempty"`
	Verified string    `json:"verified"` // signed, bad, changed (msg.Verified*)
	// Sibling is a version another node numbered the same at the same time: both are kept, the
	// earlier one is the version of that number, this one is shown beside it.
	Sibling bool `json:"sibling,omitempty"`

	rec msg.ShareRecord
	key []byte
}

// Entry is one name in the space with its versions, oldest first.
type Entry struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Latest   Version   `json:"latest"`
	Versions []Version `json:"versions"`
	Conflict bool      `json:"conflict,omitempty"` // two versions of one number: written on two nodes at once
}

// Listing is what List returns.
type Listing struct {
	Thread       string    `json:"thread"`
	Expires      time.Time `json:"expires"`
	Bytes        int64     `json:"bytes"`
	Max          int64     `json:"max"`
	Participants []string  `json:"participants"`
	HasKey       bool      `json:"has_key"`
	Entries      []Entry   `json:"entries"`
	Unreadable   int       `json:"unreadable,omitempty"` // versions this machine cannot open (no key, or not the thread's)
}

// verdict judges a version's signature against the key its writer's machine signs with.
func (s *Space) verdict(v msg.ShareVersion) string {
	if !verifySig(v.SigKey, v.Sig, v.SignedBytes(s.Thread)) {
		return msg.VerifiedBad
	}
	host := msg.HostOf(v.By)
	switch want := s.signKeyOf(host); {
	case want == "":
		_ = config.PinMachine(host, v.SigKey, "") // the first key seen for that machine, as for messages
		return msg.VerifiedSigned
	case want != v.SigKey:
		return msg.VerifiedChanged
	}
	return msg.VerifiedSigned
}

// List reads the space as the node holds it now. It also seals the key for participants that lack it.
func (s *Space) List() (Listing, error) {
	if err := s.reload(); err != nil {
		return Listing{}, err
	}
	if s.Has() {
		if err := s.Reseal(); err != nil {
			return Listing{}, err
		}
	}
	l := Listing{Thread: s.Thread, Expires: s.idx.Expires, Bytes: s.idx.Bytes, Max: s.idx.Max, Participants: s.Participants(), HasKey: s.Has(), Entries: []Entry{}}
	groups := map[string]*Entry{}
	var order []string
	for _, r := range s.idx.Records {
		v := r.Ver
		if v == nil {
			continue
		}
		k, ok := s.keys[v.KeyID]
		if !ok {
			l.Unreadable++
			continue
		}
		m, err := openMeta(k, s.Thread, v.ID, v.Meta)
		if err != nil || nameID(k, m.Kind, m.Name) != v.Name || ValidName(m.Name) != nil || (m.Kind == KindNotes) != v.Notes {
			l.Unreadable++
			continue
		}
		key := nameKind(m.Kind, m.Name)
		e := groups[key]
		if e == nil {
			kind := m.Kind
			if kind == KindDiff {
				kind = KindFile
			}
			e = &Entry{Name: m.Name, Kind: kind}
			groups[key] = e
			order = append(order, key)
		}
		e.Versions = append(e.Versions, Version{Num: v.Num, ID: v.ID, Name: m.Name, Kind: m.Kind, Size: m.Size, SHA256: m.SHA256, By: v.By, At: r.At,
			Base: v.Base, Verified: s.verdict(*v), rec: r, key: k})
	}
	for _, key := range order {
		e := groups[key]
		sort.SliceStable(e.Versions, func(i, j int) bool {
			a, b := e.Versions[i], e.Versions[j]
			if a.Num != b.Num {
				return a.Num < b.Num
			}
			if !a.At.Equal(b.At) {
				return a.At.Before(b.At)
			}
			return a.ID < b.ID
		})
		for i := 1; i < len(e.Versions); i++ {
			if e.Versions[i].Num == e.Versions[i-1].Num {
				e.Versions[i].Sibling, e.Conflict = true, true
			}
		}
		for _, v := range e.Versions {
			if !v.Sibling {
				e.Latest = v
			}
		}
		l.Entries = append(l.Entries, *e)
	}
	sort.SliceStable(l.Entries, func(i, j int) bool {
		if (l.Entries[i].Kind == KindNotes) != (l.Entries[j].Kind == KindNotes) {
			return l.Entries[i].Kind == KindNotes
		}
		return l.Entries[i].Name < l.Entries[j].Name
	})
	return l, nil
}

// AgentScreen is the Screen for an agent reader: a version msg.HoldReason objects to is held back,
// unless the person released it, and noted for `aivoicebus held` under thread and name.
func AgentScreen(thread, name string) func(id string, m msg.Message) string {
	return func(id string, m msg.Message) string {
		why := msg.HoldReason(m)
		if why == "" || config.Released(id) {
			return ""
		}
		config.NoteHeldShare(id, why, thread, name)
		return why
	}
}

// HeldID is the id a held version goes by in `aivoicebus held` and `unhold`: the first ten hex
// digits of its version id, shaped like a message id.
func HeldID(versionID string) string { return versionID[:10] }

// Get returns version num of name (0: the latest; a sibling by its id with vid) and its bytes,
// checked against the hash its writer sealed. An agent reader's Screen may hold it back.
func (s *Space) Get(name, kind string, num int, vid string) (Version, []byte, error) {
	if !s.parts[s.host] && !s.Has() {
		return Version{}, nil, s.notInThread()
	}
	l, err := s.List()
	if err != nil {
		return Version{}, nil, err
	}
	if !s.Has() {
		return Version{}, nil, s.noKey()
	}
	var e *Entry
	for i := range l.Entries {
		if l.Entries[i].Name == name && (l.Entries[i].Kind == KindNotes) == (kind == KindNotes) {
			e = &l.Entries[i]
		}
	}
	if e == nil {
		if kind == KindNotes {
			return Version{}, nil, fmt.Errorf("thread %s has no notes yet: start them with `aivoicebus share notes %s --edit --base 0`", s.Thread, s.Thread)
		}
		return Version{}, nil, fmt.Errorf("thread %s's shared space holds nothing called %q: `aivoicebus share ls %s` lists it", s.Thread, name, s.Thread)
	}
	v := e.Latest
	if num > 0 || vid != "" {
		found := false
		for _, x := range e.Versions {
			if (vid != "" && strings.HasPrefix(x.ID, vid)) || (vid == "" && x.Num == num && !x.Sibling) {
				v, found = x, true
				break
			}
		}
		if !found {
			return Version{}, nil, fmt.Errorf("%s has no version %s: `aivoicebus share ls %s` lists its versions", name, strings.TrimSpace(fmt.Sprint(num)+" "+vid), s.Thread)
		}
	}
	data, err := s.fetch(v)
	if err != nil {
		return v, nil, err
	}
	if s.Screen != nil && msg.HostOf(v.By) != s.host {
		id := HeldID(v.ID)
		if why := s.Screen(id, msg.Message{ID: id, From: v.By, Body: string(data), Verified: v.Verified}); why != "" {
			return v, nil, &HeldError{ID: id, Why: why}
		}
	}
	return v, data, nil
}

// fetch reads and opens every chunk of v, from whichever node has it.
func (s *Space) fetch(v Version) ([]byte, error) {
	rv := v.rec.Ver
	var out []byte
	for i, sha := range rv.Chunks {
		var sealed []byte
		var last error
		for _, u := range s.C.URLs {
			one := &client.Client{URLs: []string{u}, Token: s.C.Token, Timeout: 2 * time.Minute, Deadline: s.C.Deadline}
			b, err := one.ShareChunk(s.Thread, sha)
			if err == nil {
				h := sha256.Sum256(b)
				if hex.EncodeToString(h[:]) == sha {
					sealed = b
					break
				}
				err = errors.New("a node sent a chunk that does not match its hash") // nextstep: the next node is asked; the caller says try again
			}
			last = err
		}
		if sealed == nil {
			return nil, fmt.Errorf("version %d of %s cannot be read from any node (%v): try again in a minute", v.Num, v.Name, last)
		}
		pt, err := openChunk(v.key, s.Thread, rv.ID, i, len(rv.Chunks), sealed)
		if err != nil {
			return nil, fmt.Errorf("version %d of %s %v (altered, or not written with it): ask its writer (%s) to put it again", v.Num, v.Name, err, v.By)
		}
		out = append(out, pt...)
	}
	sum := sha256.Sum256(out)
	if int64(len(out)) != v.Size || hex.EncodeToString(sum[:]) != v.SHA256 {
		return nil, fmt.Errorf("version %d of %s does not match the size and hash its writer sealed: ask its writer (%s) to put it again", v.Num, v.Name, v.By)
	}
	return out, nil
}

// Describe writes one line about the space's state for a person or an agent.
func Describe(w io.Writer, l Listing, now time.Time) {
	left := l.Expires.Sub(now).Round(time.Hour)
	fmt.Fprintf(w, "shared space of thread %s: %d item(s), %s of %d MiB, kept until %s (%s from now, or 7 days after the thread's last message)\n",
		l.Thread, len(l.Entries), sizeText(l.Bytes), l.Max>>20, l.Expires.Local().Format("2006-01-02 15:04"), left)
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// SizeText is a size for a person.
func SizeText(n int64) string { return sizeText(n) }
