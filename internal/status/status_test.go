package status

import (
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	cases := map[string][3]int{"4.6.5": {4, 6, 5}, "5.0": {5, 0, 0}, "10.20.30": {10, 20, 30}}
	for in, want := range cases {
		a, b, c, err := ParseVersion(in)
		if err != nil || a != want[0] || b != want[1] || c != want[2] {
			t.Errorf("ParseVersion(%q) = %d.%d.%d, %v; want %v", in, a, b, c, err, want)
		}
	}
	for _, bad := range []string{"", "v4.6", "4", "4.6.5.1", "4.6-beta", "a.b"} {
		if _, _, _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) should fail", bad)
		}
	}
}

func TestCompare(t *testing.T) {
	if Compare(Version{5, 0, 8}, Version{5, 0, 7}) <= 0 {
		t.Error("5.0.8 should be newer than 5.0.7")
	}
	if Compare(Version{5, 1, 0}, Version{5, 0, 9}) <= 0 {
		t.Error("minor bump should be newer")
	}
	if Compare(Version{4, 6, 5}, Version{4, 6, 5}) != 0 {
		t.Error("equal")
	}
}

func TestCompute(t *testing.T) {
	v507 := Version{5, 0, 7}
	v508 := Version{5, 0, 8}
	t0 := time.Unix(1000, 0)
	mk := func(ver Version, id int64, r Rating, at int64) Vote {
		return Vote{VersionID: id, Version: ver, Rating: r, At: time.Unix(at, 0)}
	}
	_ = t0

	if got := Compute(nil); got.Rating != Unknown || got.VersionID != 0 {
		t.Errorf("empty: %+v", got)
	}

	// A single report on a newer version overrules many on an older one.
	votes := []Vote{
		mk(v507, 1, Playable, 1), mk(v507, 1, Playable, 2), mk(v507, 1, Playable, 3),
		mk(v508, 2, DoesNotLaunch, 4),
	}
	if got := Compute(votes); got.Rating != DoesNotLaunch || got.VersionID != 2 || got.Votes != 1 {
		t.Errorf("newest version wins: %+v", got)
	}

	// Majority on the newest version.
	votes = []Vote{
		mk(v508, 2, Playable, 10), mk(v508, 2, Playable, 11), mk(v508, 2, InGameIssues, 12),
		mk(v507, 1, Gold, 13),
	}
	if got := Compute(votes); got.Rating != Playable || got.Votes != 3 {
		t.Errorf("majority: %+v", got)
	}

	// Tie broken by most recent vote.
	votes = []Vote{
		mk(v508, 2, Playable, 10), mk(v508, 2, InGameIssues, 20),
	}
	if got := Compute(votes); got.Rating != InGameIssues {
		t.Errorf("tie -> most recent: %+v", got)
	}
	votes = []Vote{
		mk(v508, 2, Playable, 30), mk(v508, 2, InGameIssues, 20),
	}
	if got := Compute(votes); got.Rating != Playable {
		t.Errorf("tie -> most recent (2): %+v", got)
	}

	// +1s weigh in: one report with three +1s beats two unweighted reports.
	w := func(v Vote, plusOnes int) Vote { v.Weight = 1 + plusOnes; return v }
	votes = []Vote{
		mk(v508, 2, Playable, 10), mk(v508, 2, Playable, 11),
		w(mk(v508, 2, DoesNotLaunch, 12), 3),
	}
	if got := Compute(votes); got.Rating != DoesNotLaunch || got.Votes != 6 {
		t.Errorf("weighted majority: %+v", got)
	}
	// Two reports: the one with +1s wins.
	votes = []Vote{mk(v508, 2, Playable, 20), w(mk(v508, 2, InGameIssues, 10), 2)}
	if got := Compute(votes); got.Rating != InGameIssues {
		t.Errorf("weighted pair: %+v", got)
	}
	// +1s on an older version never count against the newest version.
	votes = []Vote{w(mk(v507, 1, Gold, 1), 10), mk(v508, 2, DoesNotLaunch, 2)}
	if got := Compute(votes); got.Rating != DoesNotLaunch || got.Votes != 1 {
		t.Errorf("weight on older version ignored: %+v", got)
	}
}
