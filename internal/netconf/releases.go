package netconf

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
)

// ClientRelease lists the releases of one client that ship a network's fork schedule. It comes only
// from the operator's release file (docs/operations.md, fork runbook); readiness itself never depends
// on it, only the "known ready release" label does.
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
	// UnscheduledIn holds, per release line, the first release that drops a cancelled fork again, so
	// versions from MinVersions up to it are the ones that still schedule it.
	UnscheduledIn []string `json:"unscheduled_in,omitempty" yaml:"unscheduled_in,omitempty"`
	Prerelease    string   `json:"prerelease,omitempty" yaml:"prerelease,omitempty"`
	Released      string   `json:"released,omitempty" yaml:"released,omitempty"`
	URL           string   `json:"url,omitempty" yaml:"url,omitempty"`
	Latest        string   `json:"latest,omitempty" yaml:"latest,omitempty"`
	LatestURL     string   `json:"latest_url,omitempty" yaml:"latest_url,omitempty"`
	Outdated      bool     `json:"outdated,omitempty" yaml:"-"`
}

type ClientReleaseTable struct {
	Updated  string          `json:"updated" yaml:"updated"`
	Releases []ClientRelease `json:"releases" yaml:"releases"`
}

var (
	releasesMu        sync.RWMutex
	clientReleases    ClientReleaseTable
	notices           []Notice
	clientReleasesGen uint64
)

// ClientReleasesGeneration changes whenever the release table or the notices change.
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

// SetForkFileData replaces the release table and the notices together, after validating both.
func SetForkFileData(t ClientReleaseTable, n []Notice) error {
	if err := errors.Join(t.Validate(), ValidateNotices(n)); err != nil {
		return err
	}
	releasesMu.Lock()
	defer releasesMu.Unlock()
	clientReleases, notices = t, n
	clientReleasesGen++
	return nil
}

func releaseKey(r ClientRelease) string {
	return r.Network + "/" + strings.ToLower(r.Fork) + "/" + r.Layer + "/" + r.Client
}

func (t ClientReleaseTable) Validate() error {
	var errs []error
	if t.Updated != "" && !isDate(t.Updated) {
		errs = append(errs, fmt.Errorf("updated %q is not a YYYY-MM-DD date", t.Updated))
	}
	seen := map[string]bool{}
	for _, r := range t.Releases {
		key := releaseKey(r)
		switch {
		case r.Network == "" || r.Fork == "":
			errs = append(errs, fmt.Errorf("%s: network and fork are required", key))
		case r.Released != "" && !isDate(r.Released):
			errs = append(errs, fmt.Errorf("%s: released %q is not a YYYY-MM-DD date", key, r.Released))
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
		case clientname.Canonical(r.Layer, r.Client) != r.Client || !clientname.Recognized(r.Layer, r.Client):
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
		// A client may ship the fix only as a pre-release before the fork time. Only its core version
		// is compared, and nodes on a pre-release build are labelled dev builds whatever the floor says.
		for _, v := range r.UnscheduledIn {
			if _, ok := semverCore(v); !ok {
				errs = append(errs, fmt.Errorf("%s: unscheduled_in version %q is not a version", key, v))
			}
		}
		if len(r.UnscheduledIn) > 0 && len(r.MinVersions) == 0 && r.Prerelease == "" {
			errs = append(errs, fmt.Errorf("%s: unscheduled_in needs the min_versions or prerelease that scheduled the fork", key))
		}
	}
	return errors.Join(errs...)
}

func isDate(s string) bool {
	_, err := time.Parse(time.DateOnly, s)
	return err == nil
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
	// For a cancelled fork: a version that still schedules it, or one that does not.
	ReleaseAffected   = "affected"
	ReleaseUnaffected = "unaffected"
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

// CancelledReleaseStatus labels a version against a cancelled fork. A version is affected when it
// reaches the floor that shipped the schedule on its line, and not the floor that drops it again. A
// fix floor clears a version only when it is not older than the scheduling floor the version reached,
// so a fix backported to an older line never clears a newer line that still schedules the fork.
func CancelledReleaseStatus(scheduled, unscheduled []string, version string) string {
	switch s := ReleaseStatus(scheduled, version); s {
	case ReleaseMeets:
		got, _ := semverCore(version)
		reached, _ := lineFloor(scheduled, got)
		if fix, ok := lineFloor(unscheduled, got); ok && !semverLess(got, fix) && !semverLess(fix, reached) {
			return ReleaseUnaffected
		}
		return ReleaseAffected
	case ReleaseBelow:
		return ReleaseUnaffected
	default:
		return s
	}
}

// lineFloor is the floor on the version's own major.minor line, or else the highest floor.
func lineFloor(floors []string, got [3]int) ([3]int, bool) {
	var highest [3]int
	found := false
	for _, f := range floors {
		floor, ok := semverCore(f)
		if !ok {
			continue
		}
		if floor[0] == got[0] && floor[1] == got[1] {
			return floor, true
		}
		if !found || semverLess(highest, floor) {
			highest, found = floor, true
		}
	}
	return highest, found
}
