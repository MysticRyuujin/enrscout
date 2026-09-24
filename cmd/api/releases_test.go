package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
)

func TestReleasesFileLoadsReloadsAndKeepsLastGood(t *testing.T) {
	t.Cleanup(func() {
		if err := netconf.SetClientReleases(netconf.BuiltinClientReleases()); err != nil {
			t.Fatal(err)
		}
	})
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
	target, err := netconf.ForkTargetAt("sepolia", time.Unix(1791294816-86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	updated := func() string {
		u, _, _ := netconf.ClientReleasesAt("sepolia", target)
		return u
	}

	start := time.Unix(1_790_000_000, 0)
	write(`{"updated":"2026-09-25","releases":[{"fork":"Glamsterdam","network":"sepolia","layer":"el","client":"Besu",
		"fork_time":1791294816,"min_versions":["26.9.0"]}]}`, start)
	f := &releasesFile{path: path}
	if err := f.load(); err != nil {
		t.Fatal(err)
	}
	if updated() != "2026-09-25" {
		t.Fatalf("file table not in use (updated %q)", updated())
	}

	write(`{"updated":"2026-09-26","releases":[{"fork":"Glamsterdam","network":"sepolia","layer":"el","client":"besu"}]}`, start.Add(time.Minute))
	if err := f.load(); err == nil {
		t.Fatal("non-canonical client name accepted")
	}
	write(`{"updated":"2026-09-26","releases":[],"typo":1}`, start.Add(2*time.Minute))
	if err := f.load(); err == nil {
		t.Fatal("unknown field accepted")
	}
	if updated() != "2026-09-25" {
		t.Fatalf("a rejected file replaced the good table (updated %q)", updated())
	}

	write(`{"updated":"2026-09-27","releases":[]}`, start.Add(3*time.Minute))
	if err := f.load(); err != nil || updated() != "2026-09-27" {
		t.Fatalf("corrected file not reloaded: %v, updated %q", err, updated())
	}
}

func TestDecodeReleasesYAMLWithComments(t *testing.T) {
	table, err := decodeReleases([]byte(`# Operator override: Besu shipped the Sepolia schedule.
updated: 2026-09-25
releases:
  - fork: Glamsterdam
    network: sepolia
    layer: el
    client: Besu
    fork_time: 1791294816 # Sepolia Glamsterdam, checked in the tagged genesis
    min_versions: ["26.9.0"]
    released: 2026-09-25
`))
	if err != nil {
		t.Fatal(err)
	}
	if table.Updated != "2026-09-25" || len(table.Releases) != 1 || table.Releases[0].Released != "2026-09-25" ||
		table.Releases[0].ForkTime != 1791294816 || table.Releases[0].MinVersions[0] != "26.9.0" {
		t.Fatalf("decoded %+v", table)
	}
	if err := table.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"misspelt key":    "updated: x\nrelease: []\n",
		"second document": "updated: x\nreleases: []\n---\nupdated: y\n",
		"response field":  "updated: x\nreleases:\n  - {fork: G, network: sepolia, layer: el, client: Geth, outdated: true}\n",
	} {
		if _, err := decodeReleases([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
