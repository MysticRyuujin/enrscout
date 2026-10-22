package forkfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netconf/netconftest"
)

func resetClientReleases(t *testing.T) {
	t.Cleanup(func() {
		if err := netconf.SetForkFileData(netconf.ClientReleaseTable{}, nil); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWatcherLoadsReloadsAndKeepsLastGood(t *testing.T) {
	resetClientReleases(t)
	path := filepath.Join(t.TempDir(), "releases.json")
	write := func(body string, mod time.Time) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, mod, mod); err != nil {
			t.Fatal(err)
		}
	}
	target := netconftest.Target(t)
	entries := func() (string, []netconf.ClientRelease) {
		return netconf.ClientReleasesAt(netconftest.Network, target)
	}

	start := time.Unix(1_790_000_000, 0)
	write(`{"updated":"2026-09-25","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu","fork_time":1793036568,"min_versions":["26.9.9"]},
		{"fork":"Glamsterdam","network":"hoodi","layer":"cl","client":"Teku","fork_time":1793036568,"min_versions":["26.10.1"]}]}`, start)
	gen := netconf.ClientReleasesGeneration()
	f, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if updated, rs := entries(); updated != "2026-09-25" || len(rs) != 2 || rs[0].Client != "Besu" || rs[0].MinVersions[0] != "26.9.9" {
		t.Fatalf("table = %q %+v, want exactly the file's two entries", updated, rs)
	}
	if netconf.ClientReleasesGeneration() == gen {
		t.Fatal("generation did not change, so cached responses would keep the old table")
	}

	write(`{"updated":"2026-09-26","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu"},
		{"fork":"glamsterdam","network":"hoodi","layer":"el","client":"Besu"}]}`, start.Add(30*time.Second))
	if err := f.Reload(); err == nil {
		t.Fatal("duplicate key inside the file accepted")
	}
	write(`{"updated":"2026-09-26","releases":[{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"besu"}]}`, start.Add(time.Minute))
	if err := f.Reload(); err == nil {
		t.Fatal("non-canonical client name accepted")
	}
	write(`{"updated":"2026-09-26","releases":[],"typo":1}`, start.Add(2*time.Minute))
	if err := f.Reload(); err == nil {
		t.Fatal("unknown field accepted")
	}
	write(`{"updated":"26 September","releases":[]}`, start.Add(150*time.Second))
	if err := f.Reload(); err == nil {
		t.Fatal("non-date updated accepted")
	}
	if updated, rs := entries(); updated != "2026-09-25" || len(rs) != 2 {
		t.Fatalf("a rejected file replaced the good table (updated %q, %d entries)", updated, len(rs))
	}

	write(`{"updated":"2026-09-27","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu","fork_time":1793036568,"min_versions":["26.9.9"]}]}`, start.Add(3*time.Minute))
	if err := f.Reload(); err != nil {
		t.Fatal(err)
	}
	if updated, rs := entries(); updated != "2026-09-27" || len(rs) != 1 || rs[0].Client != "Besu" {
		t.Fatalf("table = %q %+v, want the entry dropped from the file gone", updated, rs)
	}
	// Same size and mtime, as `cp -p` leaves it: only the content changed.
	write(`{"updated":"2026-09-28","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu","fork_time":1793036568,"min_versions":["26.9.9"]}]}`, start.Add(3*time.Minute))
	if err := f.Reload(); err != nil {
		t.Fatal(err)
	}
	if updated, _ := entries(); updated != "2026-09-28" {
		t.Fatalf("same-size, same-mtime edit not reloaded (updated %q)", updated)
	}
}

func TestExampleForkFileLoads(t *testing.T) {
	resetClientReleases(t)
	t.Cleanup(netconf.ResetForTest(netconftest.Network))
	if _, err := Open(filepath.Join("..", "..", "deploy", "e2e", "client-releases.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeYAMLWithComments(t *testing.T) {
	table, err := Decode([]byte(`# Operator override: Besu shipped the Hoodi schedule.
updated: 2026-09-25
releases:
  - fork: Glamsterdam
    network: hoodi
    layer: el
    client: Besu
    fork_time: 1793036568 # Hoodi Glamsterdam, checked in the tagged genesis
    min_versions: ["26.9.0"]
    released: 2026-09-25
`))
	if err != nil {
		t.Fatal(err)
	}
	if table.Updated != "2026-09-25" || len(table.Releases) != 1 || table.Releases[0].Released != "2026-09-25" ||
		table.Releases[0].ForkTime != 1793036568 || table.Releases[0].MinVersions[0] != "26.9.0" {
		t.Fatalf("decoded %+v", table)
	}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"misspelt key":    "updated: x\nrelease: []\n",
		"second document": "updated: x\nreleases: []\n---\nupdated: y\n",
		"response field":  "updated: x\nreleases:\n  - {fork: G, network: hoodi, layer: el, client: Geth, outdated: true}\n",
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

const cancelledHoodi = `updated: 2026-10-22
cancelled_forks:
  - network: hoodi
    fork: Glamsterdam
    el: {name: Amsterdam, time: 1793036568}
    cl: {name: gloas, epoch: 132352, version: "80000910"}
notices:
  - network: hoodi
    severity: warning
    title: Hoodi Glamsterdam is cancelled
    body: Downgrade or upgrade before the old activation time.
    links:
      - {label: eth-clients/hoodi#33, url: "https://github.com/eth-clients/hoodi/pull/33"}
releases: []
`

func TestOpenAppliesCancellationsAndReloadsNotices(t *testing.T) {
	resetClientReleases(t)
	t.Cleanup(netconf.ResetForTest(netconftest.Network))
	path := filepath.Join(t.TempDir(), "fork.yaml")
	if err := os.WriteFile(path, []byte(cancelledHoodi), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := netconf.ForkTargetAt(netconftest.Network, netconftest.Before); err != nil || !target.Cancelled {
		t.Fatalf("target = %+v, %v; want the cancelled Glamsterdam", target, err)
	}
	notices := netconf.NoticesAt(netconftest.Network, netconftest.Before)
	if len(notices) != 1 || notices[0].Links[0].URL != "https://github.com/eth-clients/hoodi/pull/33" {
		t.Fatalf("notices = %+v", notices)
	}
	if got := netconf.NoticesAt("mainnet", netconftest.Before); len(got) != 0 {
		t.Fatalf("a hoodi notice shows on mainnet: %+v", got)
	}

	edited := strings.Replace(cancelledHoodi, "Hoodi Glamsterdam is cancelled", "Hoodi Glamsterdam is postponed", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Reload(); err != nil || w.RestartRequired() {
		t.Fatalf("notice edit: %v, restart required %v", err, w.RestartRequired())
	}
	if got := netconf.NoticesAt(netconftest.Network, netconftest.Before); got[0].Title != "Hoodi Glamsterdam is postponed" {
		t.Fatalf("notice not reloaded: %+v", got)
	}

	mistyped := strings.Replace(edited, "time: 1793036568", "time: 1793036569", 1)
	if err := os.WriteFile(path, []byte(mistyped), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Reload(); err == nil || w.RestartRequired() {
		t.Fatalf("a cancellation that would fail at startup reloaded: %v, restart required %v", err, w.RestartRequired())
	}

	uncancelled := edited[:strings.Index(edited, "cancelled_forks:")] + edited[strings.Index(edited, "notices:"):]
	if err := os.WriteFile(path, []byte(uncancelled), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.Reload(); err != nil || !w.RestartRequired() {
		t.Fatalf("cancellation removed: %v, restart required %v; want a restart flagged", err, w.RestartRequired())
	}
	if target, _ := netconf.ForkTargetAt(netconftest.Network, netconftest.Before); !target.Cancelled {
		t.Fatal("a reload changed the schedule; cancellations apply only at startup")
	}
}

func TestOpenRejectsBadNotices(t *testing.T) {
	resetClientReleases(t)
	for name, body := range map[string]string{
		"severity":    "notices:\n  - {severity: loud, title: x}\n",
		"no title":    "notices:\n  - {severity: info}\n",
		"script link": "notices:\n  - {severity: info, title: x, links: [{label: x, url: \"javascript:alert(1)\"}]}\n",
		"network":     "notices:\n  - {severity: info, title: x, network: nowhere}\n",
		"window":      "notices:\n  - {severity: info, title: x, starts: 2026-10-23T00:00:00Z, expires: 2026-10-22T00:00:00Z}\n",
		"unknown key": "notices:\n  - {severity: info, title: x, colour: red}\n",
		"cancel typo": "cancelled_fork: []\n",
	} {
		path := filepath.Join(t.TempDir(), "fork.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
