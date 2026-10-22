package netconf

import (
	"strings"
	"testing"
	"time"
)

func TestReleaseStatus(t *testing.T) {
	cases := []struct {
		floors  []string
		version string
		want    string
	}{
		{[]string{"1.17.6"}, "v1.17.6-stable-3d84c6b2", ReleaseMeets},
		{[]string{"1.17.6"}, "v1.17.5-stable-9621c6ad", ReleaseBelow},
		{[]string{"1.17.6"}, "v1.17.7-unstable-572bd369-20260923", ReleaseDevBuild},
		{[]string{"1.17.6"}, "v1.18.0", ReleaseMeets},
		{[]string{"2.0.0"}, "v2.0.0+bec830cd-hp", ReleaseMeets},
		{[]string{"2.0.0"}, "2.0.1", ReleaseMeets},
		{[]string{"2.0.0"}, "v2.1.0-unstable+964bf134-hp", ReleaseDevBuild},
		{[]string{"2.0.0"}, "v1.39.3+28cbe2a0", ReleaseBelow},
		{[]string{"28.0.0"}, "v27.0.0-main-39497ecb72a72307c0d61521c019ce6bb03291f9", ReleaseDevBuild},
		{[]string{"28.0.0"}, "v28.0.0-rc.1", ReleaseDevBuild},
		{[]string{"2.0.0"}, "v2.0.0-mainnet", ReleaseMeets},
		{[]string{"2.0.0"}, "v2.0.0-rc1", ReleaseDevBuild},
		{[]string{"1.17.6"}, "cashcow", ReleaseUnparsable},
		// A backport line: 1.39.4 carries the fork alongside 2.0.0.
		{[]string{"1.39.4", "2.0.0"}, "v1.39.4+aa", ReleaseMeets},
		{[]string{"1.39.4", "2.0.0"}, "v1.39.5", ReleaseMeets},
		{[]string{"1.39.4", "2.0.0"}, "v1.39.3", ReleaseBelow},
		{[]string{"1.39.4", "2.0.0"}, "v1.40.0", ReleaseBelow},
		{[]string{"1.39.4", "2.0.0"}, "v2.0.1", ReleaseMeets},
		{[]string{"2.0.0", "1.39.4"}, "v2.3.0", ReleaseMeets},
	}
	for _, tc := range cases {
		if got := ReleaseStatus(tc.floors, tc.version); got != tc.want {
			t.Errorf("ReleaseStatus(%v, %q) = %q, want %q", tc.floors, tc.version, got, tc.want)
		}
	}
}

func TestClientReleaseTableValidate(t *testing.T) {
	good := ClientRelease{Fork: "Glamsterdam", Network: "hoodi", Layer: "el", Client: "Geth", ForkTime: 1, MinVersions: []string{"1.17.6"}}
	for name, tc := range map[string]struct {
		mutate func(*ClientRelease)
		err    string
	}{
		"non-canonical client": {func(r *ClientRelease) { r.Client = "geth" }, "canonical"},
		"other-layer client":   {func(r *ClientRelease) { r.Layer = "cl" }, "canonical"},
		"bad layer":            {func(r *ClientRelease) { r.Layer = "execution" }, "layer"},
		"missing fork time":    {func(r *ClientRelease) { r.ForkTime = 0 }, "fork_time"},
		"dev build floor":      {func(r *ClientRelease) { r.MinVersions = []string{"1.18.0-rc.1"} }, "not a release"},
		"unparsable floor":     {func(r *ClientRelease) { r.MinVersions = []string{"latest"} }, "not a release"},
		"unknown network":      {func(r *ClientRelease) { r.Network = "sepoila" }, "unknown network"},
		"script url":           {func(r *ClientRelease) { r.URL = "javascript:alert(1)" }, "http(s)"},
		"script latest url":    {func(r *ClientRelease) { r.LatestURL = "javascript:alert(1)" }, "http(s)"},
		"latest below floor":   {func(r *ClientRelease) { r.Latest = "1.17.5" }, "meets min_versions"},
		"latest rc":            {func(r *ClientRelease) { r.Latest = "1.17.8-rc.1" }, "meets min_versions"},
		"latest without floor": {func(r *ClientRelease) { r.MinVersions, r.Latest = nil, "1.17.7" }, "meets min_versions"},
		"released not a date":  {func(r *ClientRelease) { r.Released = "Oct 16" }, "YYYY-MM-DD"},
	} {
		r := good
		tc.mutate(&r)
		if err := (ClientReleaseTable{Releases: []ClientRelease{r}}).Validate(); err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: Validate = %v, want an error mentioning %q", name, err, tc.err)
		}
	}
	if err := (ClientReleaseTable{Releases: []ClientRelease{good, good}}).Validate(); err == nil {
		t.Error("duplicate entry accepted")
	}
	lower := good
	lower.Fork = "glamsterdam"
	if err := (ClientReleaseTable{Releases: []ClientRelease{good, lower}}).Validate(); err == nil {
		t.Error("duplicate entry differing only in fork case accepted")
	}
}

func TestClientReleasesRescheduledForkIsOutdated(t *testing.T) {
	defer func() {
		if err := SetClientReleases(ClientReleaseTable{}); err != nil {
			t.Fatal(err)
		}
	}()
	before := ClientReleasesGeneration()
	moved := ClientReleaseTable{Updated: "2026-09-25", Releases: []ClientRelease{
		{Fork: "Glamsterdam", Network: "hoodi", Layer: "el", Client: "Geth", ForkTime: hoodiGlamsterdam - 3600, MinVersions: []string{"1.17.6"}},
		{Fork: "Glamsterdam", Network: "hoodi", Layer: "cl", Client: "Prysm", ForkTime: hoodiGlamsterdam, MinVersions: []string{"7.2.0"}},
	}}
	if err := SetClientReleases(moved); err != nil {
		t.Fatal(err)
	}
	if ClientReleasesGeneration() == before {
		t.Fatal("generation did not change, so cached responses would keep the old table")
	}
	updated, got := ClientReleasesAt("hoodi", mustTarget(t, "hoodi", time.Unix(hoodiGlamsterdam-86400, 0)))
	if updated != "2026-09-25" || len(got) != 2 || !got[0].Outdated || got[1].Outdated {
		t.Fatalf("entries = %+v (updated %q), want only the entry verified against another time outdated", got, updated)
	}
	if err := SetClientReleases(ClientReleaseTable{Releases: []ClientRelease{{Layer: "el", Client: "Geth"}}}); err == nil {
		t.Fatal("invalid table accepted")
	}
	if updated, _ := ClientReleasesAt("hoodi", mustTarget(t, "hoodi", time.Unix(hoodiGlamsterdam-86400, 0))); updated != "2026-09-25" {
		t.Fatalf("an invalid table replaced the good one (updated %q)", updated)
	}
}

func mustTarget(t *testing.T, network string, at time.Time) ForkTarget {
	t.Helper()
	target, err := ForkTargetAt(network, at)
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestCancelledReleaseStatus(t *testing.T) {
	scheduled, unscheduled := []string{"1.17.9"}, []string{"1.17.10"}
	for _, tc := range []struct {
		version string
		fixed   []string
		want    string
	}{
		{"v1.17.8", unscheduled, ReleaseUnaffected},
		{"v1.17.9", unscheduled, ReleaseAffected},
		{"v1.17.10", unscheduled, ReleaseUnaffected},
		{"v1.18.0", nil, ReleaseAffected},
		{"v1.17.9-unstable-0b714d62", unscheduled, ReleaseDevBuild},
		{"cashcow", unscheduled, ReleaseUnparsable},
	} {
		if got := CancelledReleaseStatus(scheduled, tc.fixed, tc.version); got != tc.want {
			t.Errorf("%s (fixed in %v) = %q, want %q", tc.version, tc.fixed, got, tc.want)
		}
	}
	bad := ClientRelease{Fork: "Glamsterdam", Network: "hoodi", Layer: "el", Client: "Geth", ForkTime: 1, UnscheduledIn: []string{"1.17.10"}}
	if err := (ClientReleaseTable{Releases: []ClientRelease{bad}}).Validate(); err == nil {
		t.Error("unscheduled_in without min_versions accepted")
	}
}

func TestCancelledReleaseStatusBackport(t *testing.T) {
	scheduled, fixed := []string{"1.39.4", "2.0.0"}, []string{"1.39.5"}
	for version, want := range map[string]string{
		"v1.39.4": ReleaseAffected, "v1.39.5": ReleaseUnaffected, "v2.0.0": ReleaseAffected, "v2.0.1": ReleaseAffected, "v1.39.3": ReleaseUnaffected,
	} {
		if got := CancelledReleaseStatus(scheduled, fixed, version); got != want {
			t.Errorf("%s = %q, want %q", version, got, want)
		}
	}
	if got := CancelledReleaseStatus([]string{"2.0.0"}, []string{"2.1.0"}, "v2.2.0"); got != ReleaseUnaffected {
		t.Errorf("a later line after the fix line = %q, want unaffected", got)
	}
}
