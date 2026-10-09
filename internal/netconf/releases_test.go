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

func TestBuiltinClientReleasesMatchConfiguredForks(t *testing.T) {
	table := BuiltinClientReleases()
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, r := range table.Releases {
		target, err := ForkTargetAt(r.Network, time.Unix(sepoliaGlamsterdam-86400, 0))
		if err != nil {
			t.Fatal(err)
		}
		if target.Name != r.Fork {
			t.Errorf("%s/%s: no configured %s fork (target %q)", r.Network, r.Client, r.Fork, target.Name)
		}
	}
	_, got := ClientReleasesAt("sepolia", mustTarget(t, "sepolia", time.Unix(sepoliaGlamsterdam-86400, 0)))
	for _, r := range got {
		if r.Outdated {
			t.Errorf("%s: built-in entry is outdated against the configured fork time", r.Client)
		}
	}
}

func TestClientReleaseTableValidate(t *testing.T) {
	good := ClientRelease{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Geth", ForkTime: 1, MinVersions: []string{"1.17.6"}}
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
		if err := SetClientReleases(BuiltinClientReleases()); err != nil {
			t.Fatal(err)
		}
	}()
	before := ClientReleasesGeneration()
	moved := ClientReleaseTable{Updated: "2026-09-25", Releases: []ClientRelease{
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Geth", ForkTime: sepoliaGlamsterdam - 3600, MinVersions: []string{"1.17.6"}},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Prysm", ForkTime: sepoliaGlamsterdam, MinVersions: []string{"7.2.0"}},
	}}
	if err := SetClientReleases(moved); err != nil {
		t.Fatal(err)
	}
	if ClientReleasesGeneration() == before {
		t.Fatal("generation did not change, so cached responses would keep the old table")
	}
	updated, got := ClientReleasesAt("sepolia", mustTarget(t, "sepolia", time.Unix(sepoliaGlamsterdam-86400, 0)))
	if updated != "2026-09-25" || len(got) != 2 || !got[0].Outdated || got[1].Outdated {
		t.Fatalf("entries = %+v (updated %q), want only the entry verified against another time outdated", got, updated)
	}
	if err := SetClientReleases(ClientReleaseTable{Releases: []ClientRelease{{Layer: "el", Client: "Geth"}}}); err == nil {
		t.Fatal("invalid table accepted")
	}
	if updated, _ := ClientReleasesAt("sepolia", mustTarget(t, "sepolia", time.Unix(sepoliaGlamsterdam-86400, 0))); updated != "2026-09-25" {
		t.Fatalf("an invalid table replaced the good one (updated %q)", updated)
	}
}

func TestClientReleaseTableOverlay(t *testing.T) {
	geth := ClientRelease{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Geth", ForkTime: 1,
		MinVersions: []string{"1.17.6"}, Latest: "1.17.7", URL: "https://example.com/geth"}
	besu := ClientRelease{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Besu", ForkTime: 1, MinVersions: []string{"26.9.0"}}
	prysm := ClientRelease{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Prysm"}
	base := ClientReleaseTable{Updated: "2026-09-01", Releases: []ClientRelease{geth, besu, prysm}}

	newGeth := ClientRelease{Fork: "glamsterdam", Network: "sepolia", Layer: "el", Client: "Geth", ForkTime: 1, MinVersions: []string{"1.18.0"}}
	teku := ClientRelease{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Teku"}
	sameBesu := besu
	sameBesu.MinVersions = []string{"26.9.0"}
	merged, redundant := base.Overlay(ClientReleaseTable{Releases: []ClientRelease{newGeth, teku, sameBesu}})

	if merged.Updated != "2026-09-01" {
		t.Errorf("updated = %q, want the base date when the overlay sets none", merged.Updated)
	}
	if len(merged.Releases) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(merged.Releases), merged.Releases)
	}
	if got := merged.Releases[0]; got.Client != "Geth" || got.MinVersions[0] != "1.18.0" || got.Latest != "" || got.URL != "" {
		t.Errorf("slot 0 = %+v, want the overlay Geth in Geth's slot, with omitted fields cleared", got)
	}
	if merged.Releases[1].Client != "Besu" || merged.Releases[2].Client != "Prysm" || merged.Releases[3].Client != "Teku" {
		t.Errorf("order = %v, want base order with the new entry appended", merged.Releases)
	}
	if len(redundant) != 1 || redundant[0] != releaseKey(besu) {
		t.Errorf("redundant = %v, want only the unchanged Besu entry", redundant)
	}

	merged.Releases[1].MinVersions[0] = "mutated"
	if base.Releases[1].MinVersions[0] != "26.9.0" || besu.MinVersions[0] != "26.9.0" {
		t.Error("the merged table aliases the base table")
	}

	dated, _ := base.Overlay(ClientReleaseTable{Updated: "2026-10-01"})
	if dated.Updated != "2026-10-01" || len(dated.Releases) != 3 {
		t.Errorf("an empty overlay with a date gave %+v", dated)
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
