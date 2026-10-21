package msg

import (
	"strings"
	"testing"
	"time"
)

func TestANewTestIDHasTheReservedShapeAndStaysAValidId(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := NewTestID()
		if !IsTestID(id) || !ValidID(id) || !ValidRetractID(id) || !strings.HasPrefix(id, TestIDPrefix) {
			t.Fatalf("NewTestID() = %q is not a test id that every id check accepts", id)
		}
	}
}

func TestIsTestIDRefusesWhatIsNotExactlyTheShape(t *testing.T) {
	good := NewTestID()
	// the same nine digits with every other check digit
	wrong := 0
	for _, c := range "0123456789abcdef" {
		if cand := good[:9] + string(c); cand != good {
			if IsTestID(cand) {
				t.Errorf("%q has the wrong check digit and is accepted", cand)
			}
			wrong++
		}
	}
	if wrong != 15 {
		t.Fatalf("tried %d wrong check digits, want 15", wrong)
	}
	for _, bad := range []string{"", "7e57", good[:9], good + "0", strings.ToUpper(good), "0e57" + good[4:], NewID(), "7e57zzzzzz"} {
		if IsTestID(bad) {
			t.Errorf("IsTestID(%q) = true", bad)
		}
	}
}

func TestNewIDNeverMakesATestId(t *testing.T) {
	// the first draw is a test id (what an unlucky random draw would be), the second an ordinary one
	good := NewTestID()
	draws := [][]byte{mustHex(t, good), mustHex(t, "0123456789")}
	old := randRead
	defer func() { randRead = old }()
	randRead = func(b []byte) {
		copy(b, draws[0])
		if len(draws) > 1 {
			draws = draws[1:]
		}
	}
	if got := NewID(); got != "0123456789" {
		t.Fatalf("NewID() = %q after a test-shaped draw, want it to draw again and give 0123456789", got)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		b[i] = byte(nibble(s[2*i])<<4 | nibble(s[2*i+1]))
	}
	return b
}

func TestIsTestNeedsATestIdAndAShortLife(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	exp := func(d time.Duration) *time.Time { e := at.Add(d); return &e }
	id := NewTestID()
	cases := []struct {
		name string
		m    Message
		want bool
	}{
		{"client side, ttl 10m", Message{ID: id, TTL: 600}, true},
		{"client side, ttl 1m", Message{ID: id, TTL: 60}, true},
		{"client side, ttl 601s", Message{ID: id, TTL: 601}, false},
		{"client side, no ttl", Message{ID: id}, false},
		{"stamped, 10m", Message{ID: id, At: at, Exp: exp(TestLife)}, true},
		{"stamped, 10m and a second", Message{ID: id, At: at, Exp: exp(TestLife + time.Second)}, false},
		{"stamped, no At", Message{ID: id, Exp: exp(time.Minute)}, false},
		{"stamped, default life of a bulletin", Message{ID: id, At: at, Exp: exp(24 * time.Hour)}, false},
		{"an ordinary id with a short ttl", Message{ID: NewID(), TTL: 300}, false},
		{"a test id with the wrong check digit", Message{ID: id[:9] + "x", TTL: 300}, false},
		{"a lease", Message{ID: id, TTL: 300, Kind: KindLease}, false},
		{"for the person (to human)", Message{ID: id, TTL: 300, To: "mac14/human"}, false},
		{"an approval request", Message{ID: id, TTL: 300, To: "mac14", Body: ApprovalTag + " run it"}, false},
		{"a token hand", Message{ID: id, TTL: 300, To: "mac14", Body: TokenHandTag + " x"}, false},
		{"a signed decision", Message{ID: id, TTL: 300, To: "mac14/worker", Body: "approve\n" + approvalTag + "AAAA]"}, false},
		{"a pause is allowed in the lane", Message{ID: id, TTL: 300, To: "mac14/worker", Body: PauseBody(at, "x")}, true},
		{"a task", Message{ID: id, TTL: 300, To: "mac14", Kind: KindTask, Body: "calc 1+1"}, true},
	}
	for _, c := range cases {
		if got := c.m.IsTest(); got != c.want {
			t.Errorf("%s: IsTest() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsTestAckLooksAtTheIdTheAckCarries(t *testing.T) {
	id := NewTestID()
	cases := []struct {
		id   string
		want bool
	}{
		{id, true},
		{RetractPrefix + id, true},
		{RetractLeakPrefix + id, true},
		{NewID(), false},
		{RetractPrefix + NewID(), false},
		{QuarantineIDForTest, false},
		{"retract:" + id + "x", false},
	}
	for _, c := range cases {
		if got := IsTestAck(Ack{ID: c.id, By: "a/b"}); got != c.want {
			t.Errorf("IsTestAck(%q) = %v, want %v", c.id, got, c.want)
		}
		if got := (Record{Origin: "n", Seq: 1, Ack: &Ack{ID: c.id, By: "a/b"}}).IsTest(); got != c.want {
			t.Errorf("Record{Ack %q}.IsTest() = %v, want %v", c.id, got, c.want)
		}
	}
	if (Record{Origin: "n", Seq: 1}).IsTest() {
		t.Error("a record with neither message nor ack is not a test")
	}
}

// QuarantineIDForTest is the id of the placeholder the store writes for a record it cannot keep.
const QuarantineIDForTest = "quarantined"

func TestATestStubKeepsWhoWhenAndWhereButNothingElseAndStaysATest(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	exp := at.Add(time.Minute)
	m := Message{ID: NewTestID(), From: "mac14/claude.a1b2", To: "mac14/worker", Thread: "t", Re: "r", Body: "calc 17*23", At: at, Exp: &exp,
		Prio: PrioUrgent, Kind: KindTask, FromTitle: "title", FromModel: "model", Key: "k", Box: "b", Sig: "s", Verified: VerifiedSigned}
	stub := TestStub(m)
	if stub.Body != "" || stub.Key != "" || stub.Box != "" || stub.Sig != "" || stub.FromTitle != "" || stub.FromModel != "" || stub.Verified != "" {
		t.Errorf("the stub still holds something of the message: %+v", stub)
	}
	if stub.ID != m.ID || stub.From != m.From || stub.To != m.To || stub.Thread != m.Thread || stub.Re != m.Re || !stub.At.Equal(at) || stub.Exp == nil || !stub.Exp.Equal(exp) || stub.Kind != m.Kind || stub.Prio != m.Prio {
		t.Errorf("the stub lost who, to whom or when: %+v", stub)
	}
	if !stub.IsTest() || !TestPurged(stub) || TestPurged(m) {
		t.Errorf("a stub must still be a test and be known as purged, the original not: stub %v/%v original %v", stub.IsTest(), TestPurged(stub), TestPurged(m))
	}
	if err := (Record{Origin: "node-a", Seq: 7, Msg: &stub}).ValidateReplicated(); err != nil {
		t.Errorf("a stub must be a valid replicated record on every release: %v", err)
	}
}

func TestTestRefusalNamesWhatCannotBeATest(t *testing.T) {
	id := NewTestID()
	for name, m := range map[string]Message{
		"lease":    {ID: id, Kind: KindLease},
		"approval": {ID: id, To: "mac14", Body: ApprovalTag + " x"},
		"human":    {ID: id, To: "mac14/human"},
	} {
		if TestRefusal(m) == "" {
			t.Errorf("%s: a test of it was not refused", name)
		}
	}
	if why := TestRefusal(Message{ID: id, To: "mac14", Body: "calc 1+1", Kind: KindTask}); why != "" {
		t.Errorf("a plain task was refused: %s", why)
	}
}

func TestOptedInNeedsTheLabelToBeOn(t *testing.T) {
	if !OptedIn(Labels{TestLabel: TestOn}) || OptedIn(Labels{TestLabel: "off"}) || OptedIn(nil) || OptedIn(Labels{"tool": "claude"}) {
		t.Error("OptedIn must be true only for test=on")
	}
}

func TestTheNodeAdvertisesTheTestLane(t *testing.T) {
	found := false
	for _, f := range NodeFeatures() {
		found = found || f == FeatureTestLane
	}
	if !found {
		t.Errorf("NodeFeatures() lacks %q", FeatureTestLane)
	}
}
