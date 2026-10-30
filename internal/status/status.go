// Package status holds the pure rules for UTM version ordering and the
// per-game compatibility status derived from reports.
package status

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"time"
)

// Rating is the compatibility rating of a single report.
type Rating string

const (
	Unknown       Rating = "unknown"         // gray
	DoesNotLaunch Rating = "does_not_launch" // red
	InGameIssues  Rating = "in_game_issues"  // yellow
	Playable      Rating = "playable"        // green
	Gold          Rating = "gold"            // gold star
)

// AllRatings in display order (worst to best).
var AllRatings = []Rating{Unknown, DoesNotLaunch, InGameIssues, Playable, Gold}

// Valid reports whether r is a known rating.
func (r Rating) Valid() bool { return slices.Contains(AllRatings, r) }

// Ordinal returns the rank used for tie-breaking display.
func (r Rating) Ordinal() int { return slices.Index(AllRatings, r) }

// Label is the human readable rating name.
func (r Rating) Label() string {
	switch r {
	case DoesNotLaunch:
		return "Does not launch"
	case InGameIssues:
		return "In-game issues"
	case Playable:
		return "Playable"
	case Gold:
		return "Plays to the end"
	default:
		return "Unknown"
	}
}

// Color is the CSS color class suffix.
func (r Rating) Color() string {
	switch r {
	case DoesNotLaunch:
		return "red"
	case InGameIssues:
		return "yellow"
	case Playable:
		return "green"
	case Gold:
		return "gold"
	default:
		return "gray"
	}
}

// Version is a UTM release identified by its version string (x.y.z).
type Version struct {
	Major, Minor, Patch int
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?$`)

// ParseVersion parses "4.6.5" or "4.6" into its numeric parts.
func ParseVersion(s string) (major, minor, patch int, err error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0, fmt.Errorf("invalid version %q (expected e.g. 4.6.5)", s)
	}
	major, _ = strconv.Atoi(m[1])
	minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		patch, _ = strconv.Atoi(m[3])
	}
	return major, minor, patch, nil
}

// Compare orders versions: newest release is greater.
func Compare(a, b Version) int {
	return cmp.Or(cmp.Compare(a.Major, b.Major), cmp.Compare(a.Minor, b.Minor), cmp.Compare(a.Patch, b.Patch))
}

// Vote is one report. Weight is the report itself plus its +1s; zero is
// treated as one.
type Vote struct {
	VersionID int64
	Version   Version
	Rating    Rating
	At        time.Time
	Weight    int
}

// Result is the derived status of a game.
type Result struct {
	Rating    Rating
	VersionID int64 // 0 when no votes
	Votes     int   // total weight considered on that version
}

// Compute applies the rule: consider only reports on the newest version that
// has any; each report counts once plus once per +1; the rating with the
// most weight wins; ties go to the rating with the most recent report.
func Compute(votes []Vote) Result {
	if len(votes) == 0 {
		return Result{Rating: Unknown}
	}
	newest := votes[0]
	for _, v := range votes[1:] {
		if Compare(v.Version, newest.Version) > 0 {
			newest = v
		}
	}
	type tally struct {
		rating Rating
		count  int
		latest time.Time
	}
	tallies := map[Rating]*tally{}
	considered := 0
	for _, v := range votes {
		if Compare(v.Version, newest.Version) != 0 {
			continue
		}
		w := v.Weight
		if w < 1 {
			w = 1
		}
		considered += w
		t := tallies[v.Rating]
		if t == nil {
			t = &tally{rating: v.Rating}
			tallies[v.Rating] = t
		}
		t.count += w
		if v.At.After(t.latest) {
			t.latest = v.At
		}
	}
	list := make([]*tally, 0, len(tallies))
	for _, t := range tallies {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].count != list[j].count {
			return list[i].count > list[j].count
		}
		if !list[i].latest.Equal(list[j].latest) {
			return list[i].latest.After(list[j].latest)
		}
		return list[i].rating.Ordinal() < list[j].rating.Ordinal()
	})
	return Result{Rating: list[0].rating, VersionID: newest.VersionID, Votes: considered}
}
