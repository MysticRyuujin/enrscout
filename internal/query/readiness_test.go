package query

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

const sepoliaAmsterdam = 1791294816

type readinessRow struct {
	id string
	ev netconf.ReadinessEvidence
}

func readinessCorpus(t *testing.T) []readinessRow {
	t.Helper()
	target, err := netconf.ForkTargetAt("sepolia", time.Unix(sepoliaAmsterdam-1, 0))
	if err != nil {
		t.Fatal(err)
	}
	var rows []readinessRow
	add := func(ev netconf.ReadinessEvidence) {
		rows = append(rows, readinessRow{id: fmt.Sprintf("r%03d", len(rows)), ev: ev})
	}
	for _, hash := range []string{target.EL.PreHash, target.EL.PostHash, "deadbeef"} {
		for _, next := range []uint64{0, sepoliaAmsterdam, sepoliaAmsterdam + 1, 30_000_005, math.MaxUint64} {
			add(netconf.ReadinessEvidence{Layer: "el", ForkHash: hash, ForkNext: next})
		}
	}
	for _, hash := range []string{target.CL.PreDigest, target.CL.PostDigest, "12345678"} {
		for _, enrDigest := range []string{"", hash, "12345678"} {
			for _, version := range []string{"", "90000076", "90000077"} {
				for _, epoch := range []uint64{0, 353024, 353025, math.MaxUint64} {
					add(netconf.ReadinessEvidence{Layer: "cl", ForkHash: hash, ENRForkDigest: enrDigest, ENRNextForkVersion: version, ENRNextForkEpoch: epoch})
				}
			}
		}
	}
	add(netconf.ReadinessEvidence{Layer: "unknown", ForkHash: target.EL.PreHash})
	return rows
}

func TestReadinessMatchesSQLAndGo(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"sepolia"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	rows := readinessCorpus(t)
	tuples := make([]string, 0, len(rows))
	var args []any
	for _, r := range rows {
		// Unsigned values bind as strings: database/sql rejects a uint64 with the high bit set.
		tuples = append(tuples, "(?, 'sepolia', ?, ?, CAST(? AS UBIGINT), ?, ?, CAST(? AS UBIGINT))")
		args = append(args, r.id, r.ev.Layer, r.ev.ForkHash, strconv.FormatUint(r.ev.ForkNext, 10),
			r.ev.ENRForkDigest, r.ev.ENRNextForkVersion, strconv.FormatUint(r.ev.ENRNextForkEpoch, 10))
	}
	if _, err := eng.db.Exec("INSERT INTO nodes (id, network, layer, fork_hash, fork_next, enr_fork_digest, enr_next_fork_version, enr_next_fork_epoch) VALUES "+
		strings.Join(tuples, ", "), args...); err != nil {
		t.Fatal(err)
	}
	// NULL columns reach Go as zero values, so they must classify the same as the empty evidence.
	for _, layer := range []string{"el", "cl"} {
		id := "null-" + layer
		if _, err := eng.db.Exec("INSERT INTO nodes (id, network, layer, fork_hash, fork_next, enr_fork_digest, enr_next_fork_version, enr_next_fork_epoch) VALUES (?, 'sepolia', ?, NULL, NULL, NULL, NULL, NULL)", id, layer); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, readinessRow{id: id, ev: netconf.ReadinessEvidence{Layer: layer}})
	}

	activation := time.Unix(sepoliaAmsterdam, 0)
	for name, at := range map[string]time.Time{
		"a day before": activation.Add(-24 * time.Hour), "a second before": activation.Add(-time.Second),
		"at activation": activation, "late in grace": activation.Add(netconf.ForkTrackingGrace - time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			target, err := netconf.ForkTargetAt("sepolia", at)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]netconf.Readiness{}
			for _, want := range netconf.Readinesses {
				clause, args, err := Filter{Network: "sepolia", Readiness: string(want), ForkAt: at}.where([]string{"sepolia"})
				if err != nil {
					t.Fatal(err)
				}
				ids, err := eng.db.QueryContext(context.Background(), "SELECT id FROM nodes"+clause, args...)
				if err != nil {
					t.Fatalf("%s: %v", want, err)
				}
				for ids.Next() {
					var id string
					if err := ids.Scan(&id); err != nil {
						t.Fatal(err)
					}
					if prev, dup := seen[id]; dup {
						t.Errorf("%s matched both %s and %s", id, prev, want)
					}
					seen[id] = want
				}
				ids.Close()
			}
			if target.Phase() == netconf.PhaseScheduled {
				covered := map[netconf.Readiness]bool{}
				for _, r := range seen {
					covered[r] = true
				}
				if len(covered) != len(netconf.Readinesses) {
					t.Fatalf("corpus covers only %v", covered)
				}
			}
			for _, r := range rows {
				if got, want := seen[r.id], netconf.ReadinessAt(target, "sepolia", r.ev, at); got != want {
					t.Errorf("%s %+v: SQL %q, Go %q", r.id, r.ev, got, want)
				}
			}
		})
	}
}

func TestForkReadinessAggregate(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"sepolia"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	at := time.Unix(sepoliaAmsterdam-86400, 0)
	target, err := netconf.ForkTargetAt("sepolia", at)
	if err != nil {
		t.Fatal(err)
	}
	el, cl := target.EL.PreHash, target.CL.PreDigest
	fresh := at.Unix()
	_, err = eng.db.Exec(`INSERT INTO nodes (id, network, layer, fork_hash, fork_next, enr_fork_digest, enr_next_fork_version, enr_next_fork_epoch,
		client, client_version, fp_status, fp_at) VALUES
		('g1', 'sepolia', 'el', ?, ?, '', '', 0, 'Geth', 'v1.17.6-stable-3d84c6b2', 'ok', ?),
		('g2', 'sepolia', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.5-stable-9621c6ad', 'ok', ?),
		('g3', 'sepolia', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.5-stable-9621c6ad', 'ok', ?),
		('n1', 'sepolia', 'el', ?, ?, '', '', 0, 'Nethermind', 'v2.0.0+bec830cd-hp', 'ok', ?),
		('u1', 'sepolia', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.6', '', 0),
		('l1', 'sepolia', 'cl', ?, 0, ?, '90000075', CAST('18446744073709551615' AS UBIGINT), 'Lighthouse', 'v8.2.2', 'ok', ?),
		('p1', 'sepolia', 'cl', ?, 0, '', '', 0, 'Prysm', 'v7.1.8', 'ok', ?),
		('s1', 'sepolia', 'el', 'deadbeef', 0, '', '', 0, 'Geth', 'v1.16.0', 'ok', ?)`,
		el, sepoliaAmsterdam, fresh, el, fresh, el, fresh, el, sepoliaAmsterdam, fresh, el,
		cl, cl, fresh, cl, fresh, fresh)
	if err != nil {
		t.Fatal(err)
	}

	r, err := eng.ForkReadinessAt(ctx, "sepolia", at)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != netconf.PhaseScheduled || r.Fork.Name != "Glamsterdam" || len(r.Releases) == 0 {
		t.Fatalf("header = phase %q fork %q releases %d", r.Phase, r.Fork.Name, len(r.Releases))
	}
	elr := r.Layers["el"]
	if elr.Total != 6 || elr.Counts[netconf.Ready] != 2 || elr.Counts[netconf.NotReady] != 3 || elr.Counts[netconf.Stale] != 1 || elr.Unidentified[netconf.NotReady] != 1 {
		t.Fatalf("el = %+v", elr)
	}
	geth := elr.Clients[0]
	if geth.Client != "Geth" || geth.Total != 3 {
		t.Fatalf("first client = %+v", geth)
	}
	labels := map[string]string{}
	for _, v := range geth.Versions {
		labels[v.Version] = v.Release
	}
	if labels["1.17.6"] != netconf.ReleaseMeets || labels["1.17.5"] != netconf.ReleaseBelow {
		t.Fatalf("geth version labels = %v", labels)
	}
	clr := r.Layers["cl"]
	if clr.Counts[netconf.NotReady] != 1 || clr.Counts[netconf.Unknown] != 1 {
		t.Fatalf("cl = %+v", clr)
	}

	if _, err := eng.db.Exec(`UPDATE nodes SET enode='', enr='', seq=0, ip='', ip6='', tcp=0, udp=0, tcp6=0, udp6=0,
		quic=0, quic6=0, has_v4=false, has_v5=false, score=0, first_seen=0, last_seen=0, last_check=0, os='', lang='',
		capabilities='', country='', city='', lat=0, lon=0, asn=0, org='', hosting=false, dialable=false`); err != nil {
		t.Fatal(err)
	}
	// Readiness spans forks, so the default current-fork filter must not hide its stale rows.
	stale, err := eng.Nodes(ctx, Filter{Network: "sepolia", Readiness: "stale", ForkAt: at})
	if err != nil || stale.Total != 1 || stale.Nodes[0].ID != "s1" {
		t.Fatalf("readiness=stale = %+v, %v; want s1", stale, err)
	}

	none, err := eng.ForkReadinessAt(ctx, "sepolia", time.Unix(sepoliaAmsterdam, 0).Add(netconf.ForkTrackingGrace+time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if none.Fork.CL != nil {
		t.Fatalf("grace expired but CL still tracked: %+v", none.Fork)
	}
}

func TestReadinessHistoryDownsamplesKeepingEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"sepolia"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	target, err := netconf.ForkTargetAt("sepolia", time.Unix(sepoliaAmsterdam-86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	if h, err := eng.ReadinessHistoryFor(ctx, "sepolia", target); err != nil || h != nil {
		t.Fatalf("missing history = %+v, %v; want nil, nil", h, err)
	}
	h := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: "sepolia", Fork: "Glamsterdam",
		ELTime: target.EL.Time, CLEpoch: target.CL.Epoch}
	for i := range 2881 {
		h.Points = append(h.Points, snapshot.ReadinessPoint{At: int64(1_790_000_000 + i*900)})
	}
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := snapshot.Layout{Prefix: "snapshots"}.ReadinessHistoryKey("sepolia", "Glamsterdam")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, key, data, "application/json"); err != nil {
		t.Fatal(err)
	}
	got, err := eng.ReadinessHistoryFor(ctx, "sepolia", target)
	if err != nil {
		t.Fatal(err)
	}
	n := len(got.Points)
	if n > maxHistoryPoints+1 || got.Points[0].At != h.Points[0].At || got.Points[n-1].At != h.Points[len(h.Points)-1].At {
		t.Fatalf("downsampled to %d points spanning %d..%d", n, got.Points[0].At, got.Points[n-1].At)
	}
}

func TestReadinessHistoryFromAnotherScheduleIsWithheld(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{"sepolia"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	target, err := netconf.ForkTargetAt("sepolia", time.Unix(sepoliaAmsterdam-86400, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Recorded by a crawler that still had the fork an hour earlier.
	h := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: "sepolia", Fork: target.Name,
		ELTime: target.EL.Time - 3600, CLEpoch: target.CL.Epoch, Points: []snapshot.ReadinessPoint{{At: 1}}}
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := snapshot.Layout{Prefix: "snapshots"}.ReadinessHistoryKey("sepolia", target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, key, data, "application/json"); err != nil {
		t.Fatal(err)
	}
	if got, err := eng.ReadinessHistoryFor(ctx, "sepolia", target); err != nil || got != nil {
		t.Fatalf("history from the old schedule = %+v, %v; want none", got, err)
	}
}
