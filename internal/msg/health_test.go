package msg

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestHealthFeaturesAreOptionalAndOldJSONStillReads(t *testing.T) {
	// what a 0.8.x node answers: no "features" at all
	var old Health
	if err := json.Unmarshal([]byte(`{"node":"n1","version":"0.8.1","log":{"n1":3},"messages":1,"acks":0,"sessions":0,"quarantined":0,"peers":[]}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Has(FeatureRetract) || len(old.Features) != 0 {
		t.Errorf("a node that advertises nothing has no features: %+v", old.Features)
	}
	b, _ := json.Marshal(Health{Node: "n1"})
	if strings.Contains(string(b), "features") {
		t.Errorf("an empty list is left out, so an old reader sees the shape it knew: %s", b)
	}
	// and what a client of today does with the new field plus one it has never heard of
	var cur Health
	_ = json.Unmarshal([]byte(`{"features":["retract","from-the-future"],"future_field":1}`), &cur)
	if !cur.Has(FeatureRetract) || !cur.Has("from-the-future") || cur.Has(FeatureQuarantine) {
		t.Errorf("Has: %+v", cur.Features)
	}
}

func TestNodeFeaturesAreSortedAndNamedLikeTheirConstants(t *testing.T) {
	got := NodeFeatures()
	if !slices.IsSorted(got) {
		t.Errorf("not sorted: %v", got)
	}
	for _, f := range []string{FeatureQuarantine, FeatureRetract, FeatureRetractDryRun} {
		if !slices.Contains(got, f) {
			t.Errorf("this release implements %q and must say so: %v", f, got)
		}
	}
	for _, f := range got {
		if f == "" || strings.ContainsAny(f, " ,\n") {
			t.Errorf("a feature name is one plain token: %q", f)
		}
	}
}
