package query

import (
	"context"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/clientname"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netconf/netconftest"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

var forkTime = uint64(netconftest.Activation.Unix())

type readinessRow struct {
	id string
	ev netconf.ReadinessEvidence
}

func readinessCorpus(t *testing.T) []readinessRow {
	t.Helper()
	target, err := netconf.ForkTargetAt(netconftest.Network, netconftest.Activation.Add(-time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var rows []readinessRow
	add := func(ev netconf.ReadinessEvidence) {
		rows = append(rows, readinessRow{id: fmt.Sprintf("r%03d", len(rows)), ev: ev})
	}
	for _, hash := range []string{target.EL.PreHash, target.EL.PostHash, "deadbeef"} {
		for _, next := range []uint64{0, forkTime, forkTime + 1, 30_000_005, math.MaxUint64} {
			add(netconf.ReadinessEvidence{Layer: "el", ForkHash: hash, ForkNext: next})
		}
	}
	for _, hash := range []string{target.CL.PreDigest, target.CL.PostDigest, "12345678"} {
		for _, enrDigest := range []string{"", hash, "12345678"} {
			for _, version := range []string{"", target.CL.Version, "ffffffff"} {
				for _, epoch := range []uint64{0, target.CL.Epoch, target.CL.Epoch + 1, math.MaxUint64} {
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
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	rows := readinessCorpus(t)
	tuples := make([]string, 0, len(rows))
	var args []any
	for _, r := range rows {
		// Unsigned values bind as strings: database/sql rejects a uint64 with the high bit set.
		tuples = append(tuples, "(?, 'hoodi', ?, ?, CAST(? AS UBIGINT), ?, ?, CAST(? AS UBIGINT))")
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
		if _, err := eng.db.Exec("INSERT INTO nodes (id, network, layer, fork_hash, fork_next, enr_fork_digest, enr_next_fork_version, enr_next_fork_epoch) VALUES (?, 'hoodi', ?, NULL, NULL, NULL, NULL, NULL)", id, layer); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, readinessRow{id: id, ev: netconf.ReadinessEvidence{Layer: layer}})
	}

	activation := netconftest.Activation
	instants := map[string]time.Time{
		"a day before": activation.Add(-24 * time.Hour), "a second before": activation.Add(-time.Second),
		"at activation": activation, "late in grace": activation.Add(netconf.ForkTrackingGrace - time.Second),
	}
	matchSQLAndGo(t, eng, rows, instants)
	t.Run("cancelled", func(t *testing.T) {
		netconftest.Cancel(t)
		matchSQLAndGo(t, eng, rows, instants)
	})
}

func matchSQLAndGo(t *testing.T, eng *Engine, rows []readinessRow, instants map[string]time.Time) {
	for name, at := range instants {
		t.Run(name, func(t *testing.T) {
			target, err := netconf.ForkTargetAt(netconftest.Network, at)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[string]netconf.Readiness{}
			for _, want := range netconf.Readinesses {
				clause, args, err := Filter{Network: netconftest.Network, Readiness: string(want), ForkAt: at}.where([]string{netconftest.Network})
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
			covered := map[netconf.Readiness]bool{}
			for _, r := range seen {
				covered[r] = true
			}
			wantCovered := []netconf.Readiness{netconf.Ready, netconf.Pending, netconf.NotReady, netconf.Unknown, netconf.Stale}
			if target.Phase() == netconf.PhaseScheduled {
				wantCovered = []netconf.Readiness{netconf.Ready, netconf.NotReady, netconf.Mismatch, netconf.Unknown, netconf.Stale}
			} else if target.Phase() == netconf.PhaseCancelled {
				wantCovered = []netconf.Readiness{netconf.Ready, netconf.NotReady, netconf.Unknown, netconf.Stale}
			}
			if len(covered) != len(wantCovered) {
				t.Fatalf("corpus covers %v, want %v", covered, wantCovered)
			}
			for _, r := range rows {
				if got, want := seen[r.id], netconf.ReadinessAt(target, netconftest.Network, r.ev, at); got != want {
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
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	defer func() {
		if err := netconf.SetClientReleases(netconf.ClientReleaseTable{}); err != nil {
			t.Fatal(err)
		}
	}()
	if err := netconf.SetClientReleases(netconf.ClientReleaseTable{Releases: []netconf.ClientRelease{
		{Fork: netconftest.Fork, Network: netconftest.Network, Layer: "el", Client: "Geth", ForkTime: forkTime, MinVersions: []string{"1.17.6"}},
	}}); err != nil {
		t.Fatal(err)
	}
	at := netconftest.Before
	target, err := netconf.ForkTargetAt(netconftest.Network, at)
	if err != nil {
		t.Fatal(err)
	}
	el, cl := target.EL.PreHash, target.CL.PreDigest
	state, err := netconf.CLForkStateAt(netconftest.Network, at)
	if err != nil {
		t.Fatal(err)
	}
	current := hex.EncodeToString(state.CurrentVersion[:])
	fresh := at.Unix()
	_, err = eng.db.Exec(`INSERT INTO nodes (id, network, layer, fork_hash, fork_next, enr_fork_digest, enr_next_fork_version, enr_next_fork_epoch,
		client, client_version, fp_status, fp_at) VALUES
		('g1', 'hoodi', 'el', ?, ?, '', '', 0, 'Geth', 'v1.17.6-stable-3d84c6b2', 'ok', ?),
		('g2', 'hoodi', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.5-stable-9621c6ad', 'ok', ?),
		('g3', 'hoodi', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.5-stable-9621c6ad', 'ok', ?),
		('n1', 'hoodi', 'el', ?, ?, '', '', 0, 'Nethermind', 'v2.0.0+bec830cd-hp', 'ok', ?),
		('u1', 'hoodi', 'el', ?, 0, '', '', 0, 'Geth', 'v1.17.6', '', 0),
		('l1', 'hoodi', 'cl', ?, 0, ?, ?, CAST('18446744073709551615' AS UBIGINT), 'Lighthouse', 'v8.2.2', 'ok', ?),
		('p1', 'hoodi', 'cl', ?, 0, '', '', 0, 'Prysm', 'v7.1.8', 'ok', ?),
		('s1', 'hoodi', 'el', 'deadbeef', 0, '', '', 0, 'Geth', 'v1.16.0', 'ok', ?),
		('o1', 'hoodi', 'el', ?, ?, '', '', 0, 'Silkworm', 'v0.1.0', 'ok', ?),
		('e1', 'hoodi', 'el', ?, ?, '', '', 0, 'enrscout', '', 'ok', ?)`,
		el, forkTime, fresh, el, fresh, el, fresh, el, forkTime, fresh, el,
		cl, cl, current, fresh, cl, fresh, fresh, el, forkTime, fresh, el, forkTime, fresh)
	if err != nil {
		t.Fatal(err)
	}

	r, err := eng.ForkReadinessAt(ctx, netconftest.Network, at)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != netconf.PhaseScheduled || r.Fork.Name != netconftest.Fork || len(r.Releases) == 0 {
		t.Fatalf("header = phase %q fork %q releases %d", r.Phase, r.Fork.Name, len(r.Releases))
	}
	elr := r.Layers["el"]
	// o1 is fresh but unrecognized, so it joins the unidentified row; e1 is our own advertiser and is
	// left out of every count.
	if elr.Total != 7 || elr.Counts[netconf.Ready] != 3 || elr.Counts[netconf.NotReady] != 3 || elr.Counts[netconf.Stale] != 1 ||
		elr.Unidentified[netconf.NotReady] != 1 || elr.Unidentified[netconf.Ready] != 1 {
		t.Fatalf("el = %+v", elr)
	}
	for _, c := range elr.Clients {
		if c.Client == "Other" || c.Client == "Silkworm" || c.Client == "enrscout" {
			t.Fatalf("unexpected client row %+v", c)
		}
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
	stale, err := eng.Nodes(ctx, Filter{Network: netconftest.Network, Readiness: "stale", ForkAt: at})
	if err != nil || stale.Total != 1 || stale.Nodes[0].ID != "s1" {
		t.Fatalf("readiness=stale = %+v, %v; want s1", stale, err)
	}
	ready, err := eng.Nodes(ctx, Filter{Network: netconftest.Network, Readiness: "ready", ForkAt: at})
	if err != nil || ready.Total != 3 {
		t.Fatalf("readiness=ready = %+v, %v; want g1, n1, o1", ready, err)
	}
	for _, n := range ready.Nodes {
		if n.ID == "e1" {
			t.Fatalf("readiness=ready lists our own advertiser: %+v", n)
		}
	}

	none, err := eng.ForkReadinessAt(ctx, netconftest.Network, netconftest.Activation.Add(netconf.ForkTrackingGrace+time.Hour))
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
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	target, err := netconf.ForkTargetAt(netconftest.Network, netconftest.Before)
	if err != nil {
		t.Fatal(err)
	}
	if h, err := eng.ReadinessHistoryFor(ctx, netconftest.Network, target); err != nil || h != nil {
		t.Fatalf("missing history = %+v, %v; want nil, nil", h, err)
	}
	h := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: netconftest.Network, Fork: netconftest.Fork,
		ELTime: target.EL.Time, CLEpoch: target.CL.Epoch}
	for i := range 2881 {
		h.Points = append(h.Points, snapshot.ReadinessPoint{At: int64(1_790_000_000 + i*900)})
	}
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := snapshot.Layout{Prefix: "snapshots"}.ReadinessHistoryKey(netconftest.Network, netconftest.Fork)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, key, data, "application/json"); err != nil {
		t.Fatal(err)
	}
	got, err := eng.ReadinessHistoryFor(ctx, netconftest.Network, target)
	if err != nil {
		t.Fatal(err)
	}
	n := len(got.Points)
	if n > maxHistoryPoints+1+int(2*snapshot.ReadinessDenseWindow/(15*time.Minute))+1 || got.Points[0].At != h.Points[0].At || got.Points[n-1].At != h.Points[len(h.Points)-1].At {
		t.Fatalf("downsampled to %d points spanning %d..%d", n, got.Points[0].At, got.Points[n-1].At)
	}
}

func TestThinHistoryKeepsTheActivationWindow(t *testing.T) {
	activation := netconftest.Activation
	start := activation.Add(-20 * 24 * time.Hour)
	var points []snapshot.ReadinessPoint
	for at := start; at.Before(activation.Add(10 * 24 * time.Hour)); {
		points = append(points, snapshot.ReadinessPoint{At: at.Unix()})
		at = at.Add(snapshot.ReadinessIntervalAt(at, activation))
	}
	got := thinHistory(points, activation)
	dense := 0
	for _, p := range got {
		if time.Unix(p.At, 0).Sub(activation).Abs() <= snapshot.ReadinessDenseWindow {
			dense++
		}
	}
	if want := int(2*snapshot.ReadinessDenseWindow/snapshot.ReadinessDenseInterval) + 1; dense < want-1 {
		t.Fatalf("kept %d points around activation, want about %d", dense, want)
	}
	if regular := len(got) - dense; regular > maxHistoryPoints+1 {
		t.Fatalf("kept %d regular points, want at most %d", regular, maxHistoryPoints+1)
	}
	if got[0].At != points[0].At || got[len(got)-1].At != points[len(points)-1].At {
		t.Fatalf("thinned history spans %d..%d, want %d..%d", got[0].At, got[len(got)-1].At, points[0].At, points[len(points)-1].At)
	}
}

func TestReadinessHistoryFromAnotherScheduleIsWithheld(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	target, err := netconf.ForkTargetAt(netconftest.Network, netconftest.Before)
	if err != nil {
		t.Fatal(err)
	}
	// Recorded by a crawler that still had the fork an hour earlier.
	h := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: netconftest.Network, Fork: target.Name,
		ELTime: target.EL.Time - 3600, CLEpoch: target.CL.Epoch, Points: []snapshot.ReadinessPoint{{At: 1}}}
	data, err := h.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := snapshot.Layout{Prefix: "snapshots"}.ReadinessHistoryKey(netconftest.Network, target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, key, data, "application/json"); err != nil {
		t.Fatal(err)
	}
	if got, err := eng.ReadinessHistoryFor(ctx, netconftest.Network, target); err != nil || got != nil {
		t.Fatalf("history from the old schedule = %+v, %v; want none", got, err)
	}
}

// The crawler counts history points per client with clientname.Charted, so it must select the same
// rows as the chart condition behind the live per-client counts.
func TestChartFingerprintMatchesSQLAndGo(t *testing.T) {
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	at := netconftest.Activation
	cutoff := at.Add(-clientname.ChartMaxFingerprintAge).Unix()
	want := map[string]bool{}
	for i, status := range []string{"ok", "stale", "failed", "pending", ""} {
		for j, fpAt := range []int64{0, cutoff - 1, cutoff, at.Unix()} {
			id := fmt.Sprintf("r%d-%d", i, j)
			if _, err := eng.db.Exec("INSERT INTO nodes (id, network, layer, fp_status, fp_at) VALUES (?, 'hoodi', 'el', NULLIF(?, ''), ?)", id, status, fpAt); err != nil {
				t.Fatal(err)
			}
			want[id] = clientname.Charted(status, fpAt, at)
		}
	}
	cond, arg := chartFingerprintConditionAt(at)
	rows, err := eng.db.Query("SELECT id, coalesce("+cond+", false) FROM nodes", arg)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var id string
		var got bool
		if err := rows.Scan(&id, &got); err != nil {
			t.Fatal(err)
		}
		seen++
		if got != want[id] {
			t.Errorf("%s: SQL %v, Go %v", id, got, want[id])
		}
	}
	if seen != len(want) {
		t.Fatalf("read %d rows, want %d", seen, len(want))
	}
}

func TestForkReadinessCancelled(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	eng, err := New(st, []string{netconftest.Network}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	netconftest.Cancel(t)
	t.Cleanup(func() {
		if err := netconf.SetForkFileData(netconf.ClientReleaseTable{}, nil); err != nil {
			t.Fatal(err)
		}
	})
	if err := netconf.SetForkFileData(netconf.ClientReleaseTable{Releases: []netconf.ClientRelease{
		{Fork: netconftest.Fork, Network: netconftest.Network, Layer: "el", Client: "Geth", ForkTime: forkTime,
			MinVersions: []string{"1.17.9"}, UnscheduledIn: []string{"1.17.10"}},
	}}, []netconf.Notice{{Network: netconftest.Network, Severity: netconf.NoticeCritical, Title: "cancelled"}}); err != nil {
		t.Fatal(err)
	}
	at := netconftest.Before
	fresh := at.Unix()
	if _, err := eng.db.Exec(`INSERT INTO nodes (id, network, layer, fork_hash, fork_next, client, client_version, fp_status, fp_at) VALUES
		('still', 'hoodi', 'el', '23aa1351', ?, 'Geth', 'v1.17.9-stable-aaaaaaaa', 'ok', ?),
		('downgraded', 'hoodi', 'el', '23aa1351', 0, 'Geth', 'v1.17.8-stable-bbbbbbbb', 'ok', ?),
		('fixed', 'hoodi', 'el', '23aa1351', 0, 'Geth', 'v1.17.10-stable-cccccccc', 'ok', ?)`,
		forkTime, fresh, fresh, fresh); err != nil {
		t.Fatal(err)
	}
	r, err := eng.ForkReadinessAt(ctx, netconftest.Network, at)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != netconf.PhaseCancelled || !r.Fork.IsCancelled() || len(r.Notices) != 1 {
		t.Fatalf("phase %q cancelled %v notices %+v", r.Phase, r.Fork.IsCancelled(), r.Notices)
	}
	el := r.Layers["el"]
	if el == nil || el.Counts[netconf.Ready] != 2 || el.Counts[netconf.NotReady] != 1 {
		t.Fatalf("el layer = %+v, want 2 unscheduled and 1 still scheduled", el)
	}
	labels := map[string]string{}
	for _, c := range el.Clients {
		for _, v := range c.Versions {
			labels[v.Version] = v.Release
		}
	}
	if labels["1.17.9"] != netconf.ReleaseAffected || labels["1.17.8"] != netconf.ReleaseUnaffected || labels["1.17.10"] != netconf.ReleaseUnaffected {
		t.Fatalf("version labels = %v", labels)
	}
	if len(r.Schedule) == 0 || !r.Schedule[len(r.Schedule)-1].Cancelled {
		t.Fatalf("schedule = %+v, want the cancelled upgrade last", r.Schedule)
	}

	stored := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: netconftest.Network, Fork: netconftest.Fork,
		ELTime: forkTime, CLEpoch: 132352, Points: []snapshot.ReadinessPoint{{At: at.Unix(), EL: map[string]int{"ready": 1}}}}
	data, err := stored.Encode()
	if err != nil {
		t.Fatal(err)
	}
	key, err := snapshot.Layout{Prefix: "snapshots"}.ReadinessHistoryKey(netconftest.Network, netconftest.Fork)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put(ctx, key, data, "application/json"); err != nil {
		t.Fatal(err)
	}
	if h, err := eng.ReadinessHistoryFor(ctx, netconftest.Network, r.Fork); err != nil || h != nil {
		t.Fatalf("the scheduled-fork history leaked into the cancelled view: %+v, %v", h, err)
	}
}
