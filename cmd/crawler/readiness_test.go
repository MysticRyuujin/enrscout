package main

import (
	"context"
	"encoding/hex"
	"reflect"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/netconf/netconftest"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

var forkTime = uint64(netconftest.Activation.Unix())

func TestRecordReadinessAppendsAcrossRestartsAndKeepsUnreadable(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	at := netconftest.Before
	nw, err := netconf.Get(netconftest.Network)
	if err != nil {
		t.Fatal(err)
	}
	hash := nw.CurrentForkIDAt(at).Hash
	rows := map[string][]nodeset.Row{netconftest.Network: {
		{ID: "a", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), ForkNext: forkTime, Client: "Geth", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "b", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), Client: "Geth", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "c", Layer: "el", ForkHash: hex.EncodeToString(hash[:])},
		{ID: "d", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), ForkNext: forkTime, Client: "enrscout", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "e", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), ForkNext: forkTime, Client: "Besu", FPStatus: "ok", FPAt: at.Add(-8 * 24 * time.Hour).Unix()},
		{ID: "f", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), Client: "op-geth", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "g", Layer: "el", ForkHash: "deadbeef", Client: "Geth", FPStatus: "ok", FPAt: at.Unix()},
	}}
	key, err := layout.ReadinessHistoryKey(netconftest.Network, netconftest.Fork)
	if err != nil {
		t.Fatal(err)
	}
	read := func() *snapshot.ReadinessHistory {
		t.Helper()
		data, err := st.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		h, err := snapshot.DecodeReadinessHistory(data)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	first := &publisher{store: st, layout: layout, networks: []string{netconftest.Network}}
	first.recordReadiness(ctx, rows, at)
	first.recordReadiness(ctx, rows, at.Add(time.Minute))
	h := read()
	if len(h.Points) != 1 || h.ELTime != forkTime {
		t.Fatalf("history = %+v, want one point for the Amsterdam time", h)
	}
	p := h.Points[0]
	if p.EL["ready"] != 2 || p.EL["not_ready"] != 3 || p.EL["stale"] != 1 {
		t.Fatalf("point = %+v", p)
	}
	// Only fresh fingerprints of recognized clients on the current fork, as in the live per-client counts.
	if want := map[string]map[string]int{"Geth": {"ready": 1, "not_ready": 1}}; !reflect.DeepEqual(p.ELClients, want) {
		t.Fatalf("EL clients = %v, want %v", p.ELClients, want)
	}

	// A new process has no memory of the first one's points.
	restarted := &publisher{store: st, layout: layout, networks: []string{netconftest.Network}}
	restarted.recordReadiness(ctx, rows, at.Add(time.Minute))
	restarted.recordReadiness(ctx, rows, at.Add(snapshot.ReadinessInterval))
	if got := len(read().Points); got != 2 {
		t.Fatalf("after restart points = %d, want 2", got)
	}

	unreadable := []byte(`{"version":99,"points":[]}`)
	if err := st.Put(ctx, key, unreadable, "application/json"); err != nil {
		t.Fatal(err)
	}
	later := &publisher{store: st, layout: layout, networks: []string{netconftest.Network}}
	later.recordReadiness(ctx, rows, at.Add(2*snapshot.ReadinessInterval))
	if data, _ := st.Get(ctx, key); string(data) != string(unreadable) {
		t.Fatalf("unreadable history was overwritten with %s", data)
	}
}

func TestRecordReadinessIsDenseAroundActivation(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	rows := map[string][]nodeset.Row{netconftest.Network: {{ID: "a", Layer: "el"}}}
	key, err := layout.ReadinessHistoryKey(netconftest.Network, netconftest.Fork)
	if err != nil {
		t.Fatal(err)
	}
	pub := &publisher{store: st, layout: layout, networks: []string{netconftest.Network}}
	activation := netconftest.Activation
	for _, at := range []time.Time{
		activation.Add(-snapshot.ReadinessDenseWindow - 10*time.Minute),
		activation.Add(-snapshot.ReadinessDenseWindow - 5*time.Minute),
		activation.Add(-time.Minute), activation, activation.Add(time.Minute), activation.Add(90 * time.Second),
	} {
		pub.recordReadiness(ctx, rows, at)
	}
	data, err := st.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	h, err := snapshot.DecodeReadinessHistory(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Points) != 4 {
		t.Fatalf("points = %d, want one before the window and one per minute inside it", len(h.Points))
	}
}

func TestRecordReadinessKeepsCancelledSeriesApart(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	scheduledKey, err := layout.ReadinessHistoryKey(netconftest.Network, netconftest.Fork)
	if err != nil {
		t.Fatal(err)
	}
	scheduled := []byte(`{"version":1,"network":"hoodi","fork":"Glamsterdam","el_time":1793036568,"cl_epoch":132352,"points":[{"at":1}]}`)
	if err := st.Put(ctx, scheduledKey, scheduled, "application/json"); err != nil {
		t.Fatal(err)
	}
	netconftest.Cancel(t)
	rows := map[string][]nodeset.Row{netconftest.Network: {
		{ID: "a", Layer: "el", ForkHash: "23aa1351", ForkNext: forkTime},
		{ID: "b", Layer: "el", ForkHash: "23aa1351"},
	}}
	pub := &publisher{store: st, layout: layout, networks: []string{netconftest.Network}}
	pub.recordReadiness(ctx, rows, netconftest.Before)
	if data, _ := st.Get(ctx, scheduledKey); string(data) != string(scheduled) {
		t.Fatalf("the scheduled series was rewritten: %s", data)
	}
	key, err := layout.ReadinessHistoryKey(netconftest.Network, netconftest.Fork+"-cancelled")
	if err != nil {
		t.Fatal(err)
	}
	data, err := st.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	h, err := snapshot.DecodeReadinessHistory(data)
	if err != nil {
		t.Fatal(err)
	}
	if !h.Cancelled || len(h.Points) != 1 || h.Points[0].EL["ready"] != 1 || h.Points[0].EL["not_ready"] != 1 {
		t.Fatalf("cancelled history = %+v", h)
	}
}
