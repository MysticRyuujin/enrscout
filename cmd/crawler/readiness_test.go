package main

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

const sepoliaAmsterdam = 1791294816

func TestRecordReadinessAppendsAcrossRestartsAndKeepsUnreadable(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	at := time.Unix(sepoliaAmsterdam-86400, 0)
	nw, err := netconf.Get("sepolia")
	if err != nil {
		t.Fatal(err)
	}
	hash := nw.CurrentForkIDAt(at).Hash
	rows := map[string][]nodeset.Row{"sepolia": {
		{ID: "a", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), ForkNext: sepoliaAmsterdam, Client: "Geth", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "b", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), Client: "Geth", FPStatus: "ok", FPAt: at.Unix()},
		{ID: "c", Layer: "el", ForkHash: hex.EncodeToString(hash[:])},
		{ID: "d", Layer: "el", ForkHash: hex.EncodeToString(hash[:]), ForkNext: sepoliaAmsterdam, Client: "enrscout", FPStatus: "ok", FPAt: at.Unix()},
	}}
	key, err := layout.ReadinessHistoryKey("sepolia", "Glamsterdam")
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

	first := &publisher{store: st, layout: layout, networks: []string{"sepolia"}}
	first.recordReadiness(ctx, rows, at)
	first.recordReadiness(ctx, rows, at.Add(time.Minute))
	h := read()
	if len(h.Points) != 1 || h.ELTime != sepoliaAmsterdam {
		t.Fatalf("history = %+v, want one point for the Amsterdam time", h)
	}
	p := h.Points[0]
	if p.EL["ready"] != 1 || p.EL["not_ready"] != 2 {
		t.Fatalf("point = %+v", p)
	}

	// A new process has no memory of the first one's points.
	restarted := &publisher{store: st, layout: layout, networks: []string{"sepolia"}}
	restarted.recordReadiness(ctx, rows, at.Add(time.Minute))
	restarted.recordReadiness(ctx, rows, at.Add(snapshot.ReadinessInterval))
	if got := len(read().Points); got != 2 {
		t.Fatalf("after restart points = %d, want 2", got)
	}

	unreadable := []byte(`{"version":99,"points":[]}`)
	if err := st.Put(ctx, key, unreadable, "application/json"); err != nil {
		t.Fatal(err)
	}
	later := &publisher{store: st, layout: layout, networks: []string{"sepolia"}}
	later.recordReadiness(ctx, rows, at.Add(2*snapshot.ReadinessInterval))
	if data, _ := st.Get(ctx, key); string(data) != string(unreadable) {
		t.Fatalf("unreadable history was overwritten with %s", data)
	}
}
