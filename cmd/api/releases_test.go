package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netconf/netconftest"
)

func resetClientReleases(t *testing.T) {
	t.Cleanup(func() {
		if err := netconf.SetClientReleases(netconf.ClientReleaseTable{}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReleasesFileLoadsReloadsAndKeepsLastGood(t *testing.T) {
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
	f := &releasesFile{path: path}
	gen := netconf.ClientReleasesGeneration()
	if err := f.load(); err != nil {
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
	if err := f.load(); err == nil {
		t.Fatal("duplicate key inside the file accepted")
	}
	write(`{"updated":"2026-09-26","releases":[{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"besu"}]}`, start.Add(time.Minute))
	if err := f.load(); err == nil {
		t.Fatal("non-canonical client name accepted")
	}
	write(`{"updated":"2026-09-26","releases":[],"typo":1}`, start.Add(2*time.Minute))
	if err := f.load(); err == nil {
		t.Fatal("unknown field accepted")
	}
	write(`{"updated":"26 September","releases":[]}`, start.Add(150*time.Second))
	if err := f.load(); err == nil {
		t.Fatal("non-date updated accepted")
	}
	if updated, rs := entries(); updated != "2026-09-25" || len(rs) != 2 {
		t.Fatalf("a rejected file replaced the good table (updated %q, %d entries)", updated, len(rs))
	}

	write(`{"updated":"2026-09-27","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu","fork_time":1793036568,"min_versions":["26.9.9"]}]}`, start.Add(3*time.Minute))
	if err := f.load(); err != nil {
		t.Fatal(err)
	}
	if updated, rs := entries(); updated != "2026-09-27" || len(rs) != 1 || rs[0].Client != "Besu" {
		t.Fatalf("table = %q %+v, want the entry dropped from the file gone", updated, rs)
	}
	// Same size and mtime, as `cp -p` leaves it: only the content changed.
	write(`{"updated":"2026-09-28","releases":[
		{"fork":"Glamsterdam","network":"hoodi","layer":"el","client":"Besu","fork_time":1793036568,"min_versions":["26.9.9"]}]}`, start.Add(3*time.Minute))
	if err := f.load(); err != nil {
		t.Fatal(err)
	}
	if updated, _ := entries(); updated != "2026-09-28" {
		t.Fatalf("same-size, same-mtime edit not reloaded (updated %q)", updated)
	}
}

func TestExampleReleasesFileLoads(t *testing.T) {
	resetClientReleases(t)
	f := &releasesFile{path: filepath.Join("..", "..", "deploy", "e2e", "client-releases.yaml")}
	if err := f.load(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeReleasesYAMLWithComments(t *testing.T) {
	table, err := decodeReleases([]byte(`# Operator override: Besu shipped the Hoodi schedule.
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
		if _, err := decodeReleases([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
