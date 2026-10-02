package netconf

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
)

// ClientRelease lists the releases of one client that ship a network's fork schedule. It is curated
// by hand as clients ship (docs/operations.md, fork runbook); readiness itself never depends on it,
// only the "known ready release" label does.
type ClientRelease struct {
	Fork    string `json:"fork" yaml:"fork"`
	Network string `json:"network" yaml:"network"`
	Layer   string `json:"layer" yaml:"layer"`
	Client  string `json:"client" yaml:"client"`
	// ForkTime is the activation, in Unix seconds, the entry was verified against. A rescheduled
	// fork no longer matches it, so its labels are withheld (Outdated) until the entry is re-checked.
	ForkTime uint64 `json:"fork_time,omitempty" yaml:"fork_time,omitempty"`
	// MinVersions holds the first fork-ready release of each release line, so a backport such as
	// 1.39.4 alongside 2.0.0 is not read as older than the fork-ready line.
	MinVersions []string `json:"min_versions,omitempty" yaml:"min_versions,omitempty"`
	Prerelease  string   `json:"prerelease,omitempty" yaml:"prerelease,omitempty"`
	Released    string   `json:"released,omitempty" yaml:"released,omitempty"`
	URL         string   `json:"url,omitempty" yaml:"url,omitempty"`
	Latest      string   `json:"latest,omitempty" yaml:"latest,omitempty"`
	LatestURL   string   `json:"latest_url,omitempty" yaml:"latest_url,omitempty"`
	Outdated    bool     `json:"outdated,omitempty" yaml:"-"`
}

type ClientReleaseTable struct {
	Updated  string          `json:"updated" yaml:"updated"`
	Releases []ClientRelease `json:"releases" yaml:"releases"`
}

const sepoliaGlamsterdam = 1791294816

var builtinClientReleases = ClientReleaseTable{
	Updated: "2026-10-01",
	Releases: []ClientRelease{
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Geth", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"1.17.6"}, Released: "2026-09-23", URL: "https://github.com/ethereum/go-ethereum/releases/tag/v1.17.6",
			Latest: "1.17.7", LatestURL: "https://github.com/ethereum/go-ethereum/releases/tag/v1.17.7"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Nethermind", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"2.1.0"}, Released: "2026-10-01", URL: "https://github.com/NethermindEth/nethermind/releases/tag/2.1.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Ethrex", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"28.0.0"}, Released: "2026-09-29", URL: "https://github.com/lambdaclass/ethrex/releases/tag/v28.0.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Besu", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"26.9.0"}, Released: "2026-09-25", URL: "https://github.com/besu-eth/besu/releases/tag/26.9.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Erigon", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"3.7.0"}, Released: "2026-09-25", URL: "https://github.com/erigontech/erigon/releases/tag/v3.7.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Reth", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"2.7.0"}, Released: "2026-09-28", URL: "https://github.com/paradigmxyz/reth/releases/tag/v2.7.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "el", Client: "Nimbus", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"0.4.2"}, Released: "2026-09-29", URL: "https://github.com/status-im/nimbus-eth1/releases/tag/v0.4.2"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Lighthouse", ForkTime: sepoliaGlamsterdam,
			Prerelease: "8.3.0-rc.0", Released: "2026-10-01", URL: "https://github.com/sigp/lighthouse/releases/tag/v8.3.0-rc.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Prysm", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"7.2.0"}, Released: "2026-09-28", URL: "https://github.com/OffchainLabs/prysm/releases/tag/v7.2.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Teku", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"26.9.1"}, Released: "2026-09-28", URL: "https://github.com/Consensys-Incorporated/teku/releases/tag/26.9.1"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Nimbus", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"26.9.0"}, Released: "2026-09-29", URL: "https://github.com/status-im/nimbus-eth2/releases/tag/v26.9.0",
			Latest: "26.9.1", LatestURL: "https://github.com/status-im/nimbus-eth2/releases/tag/v26.9.1"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Lodestar", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"1.49.0"}, Released: "2026-09-28", URL: "https://github.com/ChainSafe/lodestar/releases/tag/v1.49.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Grandine", ForkTime: sepoliaGlamsterdam,
			Prerelease: "3.0.0-rc.0", Released: "2026-09-30", URL: "https://github.com/grandinetech/grandine/releases/tag/3.0.0-rc.0"},
		{Fork: "Glamsterdam", Network: "sepolia", Layer: "cl", Client: "Caplin", ForkTime: sepoliaGlamsterdam,
			MinVersions: []string{"3.7.0"}, Released: "2026-09-25", URL: "https://github.com/erigontech/erigon/releases/tag/v3.7.0"},
	},
}

var (
	releasesMu        sync.RWMutex
	clientReleases    = builtinClientReleases
	clientReleasesGen uint64
)

func BuiltinClientReleases() ClientReleaseTable { return builtinClientReleases }

func ClientReleasesGeneration() uint64 {
	releasesMu.RLock()
	defer releasesMu.RUnlock()
	return clientReleasesGen
}

// SetClientReleases replaces the table after validating it, so a bad operator file never
// replaces a good table.
func SetClientReleases(t ClientReleaseTable) error {
	if err := t.Validate(); err != nil {
		return err
	}
	releasesMu.Lock()
	defer releasesMu.Unlock()
	clientReleases = t
	clientReleasesGen++
	return nil
}

func (t ClientReleaseTable) Validate() error {
	var errs []error
	seen := map[string]bool{}
	for _, r := range t.Releases {
		key := r.Network + "/" + strings.ToLower(r.Fork) + "/" + r.Layer + "/" + r.Client
		switch {
		case r.Network == "" || r.Fork == "":
			errs = append(errs, fmt.Errorf("%s: network and fork are required", key))
		case !knownNetwork(r.Network):
			errs = append(errs, fmt.Errorf("%s: unknown network %q", key, r.Network))
		case r.URL != "" && !httpURL(r.URL):
			errs = append(errs, fmt.Errorf("%s: url %q is not an http(s) URL", key, r.URL))
		case r.LatestURL != "" && !httpURL(r.LatestURL):
			errs = append(errs, fmt.Errorf("%s: latest_url %q is not an http(s) URL", key, r.LatestURL))
		case r.Latest != "" && ReleaseStatus(r.MinVersions, r.Latest) != ReleaseMeets:
			errs = append(errs, fmt.Errorf("%s: latest %q is not a release that meets min_versions", key, r.Latest))
		case r.Layer != "el" && r.Layer != "cl":
			errs = append(errs, fmt.Errorf("%s: layer %q", key, r.Layer))
		case clientname.Canonical(r.Layer, r.Client) != r.Client || !clientname.Recognized(r.Client):
			errs = append(errs, fmt.Errorf("%s: %q is not a canonical client name, so its label would never match a row", key, r.Client))
		case seen[key]:
			errs = append(errs, fmt.Errorf("%s: duplicate entry", key))
		case (len(r.MinVersions) > 0 || r.Prerelease != "") && r.ForkTime == 0:
			errs = append(errs, fmt.Errorf("%s: a release needs the fork_time it was verified against", key))
		}
		seen[key] = true
		for _, v := range r.MinVersions {
			if _, ok := semverCore(v); !ok || devBuildToken.MatchString(versionSuffix(v)) {
				errs = append(errs, fmt.Errorf("%s: min version %q is not a release version", key, v))
			}
		}
	}
	return errors.Join(errs...)
}

func knownNetwork(name string) bool {
	_, err := Get(name)
	return err == nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// ClientReleasesAt returns the entries for a network's tracked fork, marking any verified against a
// fork time the target no longer has.
func ClientReleasesAt(network string, target ForkTarget) (updated string, out []ClientRelease) {
	releasesMu.RLock()
	defer releasesMu.RUnlock()
	out = []ClientRelease{}
	for _, r := range clientReleases.Releases {
		if r.Network != network || !strings.EqualFold(r.Fork, target.Name) {
			continue
		}
		if r.ForkTime != 0 {
			var want uint64
			switch {
			case r.Layer == "el" && target.EL != nil:
				want = target.EL.Time
			case r.Layer == "cl" && target.CL != nil:
				want = uint64(target.CL.Time.Unix())
			}
			r.Outdated = r.ForkTime != want
		}
		out = append(out, r)
	}
	return clientReleases.Updated, out
}

const (
	ReleaseMeets      = "meets"
	ReleaseBelow      = "below"
	ReleaseDevBuild   = "dev_build"
	ReleaseUnparsable = "unparsable"
)

var (
	leadingSemver = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)(?:\.(\d+))?`)
	// Tokens clients use for builds that are not a tagged release. Geth's "-stable-<commit>" and
	// Nethermind's "+<commit>[-hp|-f]" decorate real releases, so a strict semver prerelease check
	// would wrongly reject them.
	devBuildToken = regexp.MustCompile(`(?i)(^|[-+._/])(rc|alpha|beta|unstable|dev|develop|nightly|main|master|pre|snapshot)\d*($|[-+._/])`)
)

func versionSuffix(v string) string {
	v = strings.TrimSpace(v)
	return v[len(leadingSemver.FindString(v)):]
}

// ReleaseStatus compares a reported version with a client's fork-ready floors. A version meets when
// it is at or above the floor of its own major.minor line, or at or above the highest floor; a newer line
// with no floor of its own is not assumed to carry a backport.
func ReleaseStatus(floors []string, version string) string {
	got, ok := semverCore(version)
	if !ok {
		return ReleaseUnparsable
	}
	if devBuildToken.MatchString(versionSuffix(version)) {
		return ReleaseDevBuild
	}
	var highest [3]int
	parsed := false
	for _, f := range floors {
		floor, ok := semverCore(f)
		if !ok {
			continue
		}
		if floor[0] == got[0] && floor[1] == got[1] && !semverLess(got, floor) {
			return ReleaseMeets
		}
		if !parsed || semverLess(highest, floor) {
			highest, parsed = floor, true
		}
	}
	if !parsed {
		return ReleaseUnparsable
	}
	if !semverLess(got, highest) {
		return ReleaseMeets
	}
	return ReleaseBelow
}

func semverLess(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

func semverCore(v string) ([3]int, bool) {
	var out [3]int
	m := leadingSemver.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return out, false
	}
	for i, part := range m[1:] {
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
