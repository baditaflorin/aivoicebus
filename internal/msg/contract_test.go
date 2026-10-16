package msg

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// feedVectors is testdata/feed-vectors.json: what the dashboard reads from a node, pinned so a change on
// either side shows up as a failing vector and not as a dashboard that quietly shows less.
type feedVectors struct {
	Version    int      `json:"version"`
	Doc        []string `json:"doc"`
	Vocabulary struct {
		Priorities      []string `json:"priorities"`
		DefaultPriority string   `json:"default_priority"`
		Kinds           []string `json:"kinds"`
		AckVia          []string `json:"ack_via"`
		Quarantine      struct{ ID, By string }
	} `json:"vocabulary"`
	Records []struct {
		Name      string          `json:"name"`
		Why       string          `json:"why"`
		Record    json.RawMessage `json:"record"`
		Valid     bool            `json:"valid"`
		Reason    string          `json:"reason"`
		Canonical bool            `json:"canonical"`
	} `json:"records"`
	Expiry []struct {
		Name      string    `json:"name"`
		Why       string    `json:"why"`
		Msg       Message   `json:"msg"`
		Now       time.Time `json:"now"`
		Expired   bool      `json:"expired"`
		ExpiresAt string    `json:"expires_at"`
	} `json:"expiry"`
	Roster []struct {
		Name   string          `json:"name"`
		Why    string          `json:"why"`
		Lists  [][]RosterEntry `json:"lists"`
		Expect []RosterEntry   `json:"expect"`
	} `json:"roster"`
	Health []struct {
		Name   string          `json:"name"`
		Why    string          `json:"why"`
		JSON   json.RawMessage `json:"json"`
		Expect struct {
			Node        string            `json:"node"`
			Version     string            `json:"version"`
			Messages    int               `json:"messages"`
			Acks        int               `json:"acks"`
			Sessions    int               `json:"sessions"`
			Quarantined int               `json:"quarantined"`
			Retracted   int               `json:"retracted"`
			Log         map[string]uint64 `json:"log"`
			Peers       int               `json:"peers"`
			PeerOK      []bool            `json:"peer_ok"`
		} `json:"expect"`
	} `json:"health"`
	Acks []struct {
		ID     string `json:"id"`
		Class  string `json:"class"`
		Target string `json:"target"`
		Leak   bool   `json:"leak"`
		Why    string `json:"why"`
	} `json:"acks"`
}

func readFeedVectors(t *testing.T) feedVectors {
	t.Helper()
	b, err := os.ReadFile("testdata/feed-vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields() // a key this test does not know is a change the dashboard must be told about
	var f feedVectors
	if err := dec.Decode(&f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheFeedVectorsAreWellFormed(t *testing.T) {
	f := readFeedVectors(t)
	if f.Version != 1 || len(f.Doc) == 0 || len(f.Records) < 15 || len(f.Expiry) < 8 || len(f.Roster) < 4 || len(f.Health) < 3 || len(f.Acks) < 6 {
		t.Fatalf("version %d, %d doc lines, %d records, %d expiry, %d roster, %d health, %d acks", f.Version, len(f.Doc), len(f.Records), len(f.Expiry), len(f.Roster), len(f.Health), len(f.Acks))
	}
	names := map[string]bool{}
	for _, c := range f.Records {
		if c.Name == "" || c.Why == "" || names[c.Name] || (!c.Valid && c.Reason == "") {
			t.Errorf("record case %q: a unique name, a reason and, for a refusal, the word of it are required", c.Name)
		}
		names[c.Name] = true
	}
}

func TestTheFeedVectorsRecordsAreWhatTheReplicationCheckAndTheWireShapeSay(t *testing.T) {
	for _, c := range readFeedVectors(t).Records {
		var r Record
		if err := json.Unmarshal(c.Record, &r); err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		err := r.ValidateReplicated()
		switch {
		case c.Valid && err != nil:
			t.Errorf("%s: refused: %v", c.Name, err)
		case !c.Valid && (err == nil || !strings.Contains(err.Error(), c.Reason)):
			t.Errorf("%s: want a refusal naming %q, got %v", c.Name, c.Reason, err)
		}
		if !c.Canonical {
			continue
		}
		var want, got any
		out, _ := json.Marshal(r)
		if json.Unmarshal(c.Record, &want) != nil || json.Unmarshal(out, &got) != nil || !reflect.DeepEqual(want, got) {
			t.Errorf("%s: the wire shape changed.\nvector %s\nnow    %s", c.Name, c.Record, out)
		}
	}
}

func TestTheFeedVectorsExpiry(t *testing.T) {
	for _, c := range readFeedVectors(t).Expiry {
		if got := c.Msg.Expired(c.Now); got != c.Expired {
			t.Errorf("%s: expired = %v at %s", c.Name, got, c.Now)
		}
		exp, ok := c.Msg.ExpiresAt()
		if c.ExpiresAt == "" && ok {
			t.Errorf("%s: expires at %s, want never", c.Name, exp)
		}
		if c.ExpiresAt != "" && (!ok || !exp.Equal(mustTime(t, c.ExpiresAt))) {
			t.Errorf("%s: expires at %s (%v), want %s", c.Name, exp, ok, c.ExpiresAt)
		}
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTheFeedVectorsRosterMergeAndHealth(t *testing.T) {
	f := readFeedVectors(t)
	for _, c := range f.Roster {
		got := MergeRosters(c.Lists...)
		if len(got) != len(c.Expect) {
			t.Errorf("%s: %+v, want %+v", c.Name, got, c.Expect)
			continue
		}
		for i := range got {
			w := c.Expect[i]
			if got[i].Me != w.Me || !got[i].Seen.Equal(w.Seen) || !reflect.DeepEqual(got[i].Labels, w.Labels) {
				t.Errorf("%s: entry %d is %+v, want %+v", c.Name, i, got[i], w)
			}
		}
	}
	for _, c := range f.Health {
		var h Health
		if err := json.Unmarshal(c.JSON, &h); err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		e := c.Expect
		ok := make([]bool, 0, len(h.Peers))
		for _, p := range h.Peers {
			ok = append(ok, p.OK)
		}
		if h.Node != e.Node || h.Version != e.Version || h.Messages != e.Messages || h.Acks != e.Acks || h.Sessions != e.Sessions ||
			h.Quarantined != e.Quarantined || h.Retracted != e.Retracted || !reflect.DeepEqual(h.Log, e.Log) ||
			len(h.Peers) != e.Peers || !reflect.DeepEqual(ok, e.PeerOK) {
			t.Errorf("%s: decoded %+v", c.Name, h)
		}
	}
}

func TestTheFeedVectorsVocabularyAndAckClasses(t *testing.T) {
	f := readFeedVectors(t)
	v := f.Vocabulary
	if !reflect.DeepEqual(v.Priorities, PrioNames()) || !reflect.DeepEqual(v.Kinds, KindNames()) || v.DefaultPriority != (Message{}).Priority() {
		t.Errorf("the vocabulary in the vectors (%v %v %q) is not the one in vocab.go (%v %v)", v.Priorities, v.Kinds, v.DefaultPriority, PrioNames(), KindNames())
	}
	for _, via := range v.AckVia {
		if via == "" || !ValidVia(via) {
			t.Errorf("via %q", via)
		}
	}
	if ValidVia("carrier-pigeon") || len(v.AckVia) != 4 {
		t.Errorf("the known ways of delivery changed: %v", v.AckVia)
	}
	for _, c := range f.Acks {
		mid, leak, isRetraction := RetractedID(c.ID)
		class := "receipt"
		switch {
		case isRetraction:
			class = "retract"
		case HasRetractPrefix(c.ID):
			class = "claimed-not-valid"
		}
		if class != c.Class || mid != c.Target || leak != c.Leak {
			t.Errorf("ack %q: class %q target %q leak %v; vector says %q %q %v", c.ID, class, mid, leak, c.Class, c.Target, c.Leak)
		}
	}
}
