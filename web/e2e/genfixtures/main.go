// Command genfixtures writes small snapshot fixtures for deterministic API/web E2E
// without a live crawl; rows carry netconf's current EL fork id and CL digest so the
// headline current-only views are non-empty (it tracks the fork config at gen time).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func main() {
	out := flag.String("out", "deploy/e2e/fixtures", "filesystem store directory to write into")
	flag.Parse()

	if err := run(*out); err != nil {
		log.Fatal(err)
	}
}

func run(out string) error {
	st, err := store.NewFS(out)
	if err != nil {
		return err
	}
	ctx := context.Background()
	layout := snapshot.Layout{}
	gen := time.Now().UTC().Truncate(time.Second)

	networks := []string{"mainnet", "hoodi", "sepolia"}
	manifestNetworks := make(map[string]snapshot.NetworkSnapshot, len(networks))

	for _, network := range networks {
		rows, err := currentRows(network, gen)
		if err != nil {
			return fmt.Errorf("%s rows: %w", network, err)
		}
		data, err := nodeset.ParquetFromRows(rows)
		if err != nil {
			return fmt.Errorf("%s parquet: %w", network, err)
		}
		sum := sha256.Sum256(data)
		key := layout.GenerationKey(network, gen)
		if err := st.Put(ctx, key, data, "application/vnd.apache.parquet"); err != nil {
			return err
		}

		el, cl := 0, 0
		for _, r := range rows {
			if r.Layer == "cl" {
				cl++
			} else {
				el++
			}
		}
		manifestNetworks[network] = snapshot.NetworkSnapshot{
			GenerationKey: key, NodeCount: len(rows), CurrentNodeCount: len(rows), SHA256: hex.EncodeToString(sum[:]), Bytes: len(data),
		}
	}

	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion,
		GeneratedAt:   gen,
		CrawlerID:     "e2e-fixture",
		Run: snapshot.RunMetadata{
			RunID:                "e2e-fixture-run",
			SourceRevision:       "e2e-fixture",
			SourceURL:            "https://github.com/MysticRyuujin/enrscout",
			ConfigSHA256:         strings.Repeat("00", sha256.Size),
			CrawlerStartedAt:     gen.Add(-72 * time.Hour),
			MethodologyStartedAt: gen.Add(-72 * time.Hour),
			MethodologyVersion:   snapshot.MethodologyVersion,
			MethodologyID:        "e2e-fixture-method",
		},
		Networks: manifestNetworks,
	}
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		return err
	}
	fmt.Printf("wrote %d-network fixtures to %s (generated_at %s)\n", len(networks), out, gen.Format(time.RFC3339))
	return nil
}

func currentRows(network string, gen time.Time) ([]nodeset.Row, error) {
	nw, err := netconf.Get(network)
	if err != nil {
		return nil, err
	}
	elFork := nw.CurrentForkIDAt(gen)
	elHash := hex.EncodeToString(elFork.Hash[:])
	clState, err := netconf.CLForkStateAt(network, gen)
	if err != nil {
		return nil, err
	}
	clDigest := hex.EncodeToString(clState.Digest[:])
	ts := gen.Unix()

	next := 0
	id := func() string {
		next++
		h := sha256.Sum256(fmt.Appendf(nil, "%s-%d", network, next))
		return hex.EncodeToString(h[:])
	}

	el := []struct{ client, version, os string }{
		{"Geth", "1.15.0", "linux"},
		{"Nethermind", "1.30.0", "linux"},
		{"reth", "1.3.0", "linux"},
		{"Besu", "25.7.0", "windows"},
	}
	cl := []struct{ client, version, os string }{
		{"Lighthouse", "6.0.0", "linux"},
		{"Prysm", "5.2.0", "linux"},
		{"Teku", "25.7.0", "linux"},
	}

	rows := make([]nodeset.Row, 0, len(el)+len(cl))
	for i, c := range el {
		rows = append(rows, nodeset.Row{
			ID: id(), Network: network, Layer: "el", ForkHash: elHash, ForkNext: 0,
			IP: fmt.Sprintf("203.0.113.%d", i+1), TCP: 30303, UDP: 30303, HasV4: true,
			Client: c.client, Version: c.version, OS: c.os, Caps: "eth/68",
			Country: "US", City: "Ashburn", Lat: 39.04, Lon: -77.48, Geolocated: true, GeoAccuracyRadiusKM: 20,
			ASN: 16509, Org: "Amazon", Hosting: true, HostingKnown: true,
			Score: 100, FirstSeen: ts - 3600, LastSeen: ts, LastCheck: ts, LastResolved: ts,
			FPStatus: "ok", FPAt: ts, FPDirection: "inbound", MembershipSource: "status", MembershipVerifiedAt: ts, ForkSource: "status", ForkObservedAt: ts,
		})
	}
	for i, c := range cl {
		rows = append(rows, nodeset.Row{
			ID: id(), Network: network, Layer: "cl", ForkHash: clDigest,
			IP: fmt.Sprintf("198.51.100.%d", i+1), TCP: 9000, UDP: 9000, QUIC: 9001, HasV5: true,
			Client: c.client, Version: c.version, OS: c.os,
			Country: "DE", City: "Frankfurt", Lat: 50.11, Lon: 8.68, Geolocated: true, GeoAccuracyRadiusKM: 20,
			ASN: 24940, Org: "Hetzner", Hosting: true, HostingKnown: true,
			Score: 100, FirstSeen: ts - 3600, LastSeen: ts, LastCheck: ts, LastResolved: ts,
			FPStatus: "ok", FPAt: ts, FPDirection: "outbound", MembershipSource: "status", MembershipVerifiedAt: ts, ForkSource: "status", ForkObservedAt: ts,
		})
	}
	return rows, nil
}
