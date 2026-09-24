package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/query"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func forksTestEngine(t *testing.T, networks []string) (*query.Engine, store.Store) {
	t.Helper()
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	gen := time.Now().Truncate(time.Second)
	m := &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, GeneratedAt: gen, CrawlerID: "test-crawler",
		Run: snapshot.RunMetadata{
			RunID: "test-run", SourceRevision: "test-revision", SourceURL: "https://example.com/source",
			ConfigSHA256: hex.EncodeToString(make([]byte, sha256.Size)), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion, MethodologyID: "test-method",
		},
		Networks: map[string]snapshot.NetworkSnapshot{},
	}
	for _, network := range networks {
		nw, err := netconf.Get(network)
		if err != nil {
			t.Fatal(err)
		}
		fork := nw.CurrentForkIDAt(gen)
		data, err := nodeset.ParquetFromRows([]nodeset.Row{{
			ID: "id-" + network, Network: network, Layer: "el", IP: "1.1.1.1", TCP: 30303,
			ForkHash: hex.EncodeToString(fork.Hash[:]), ForkNext: fork.Next,
		}})
		if err != nil {
			t.Fatal(err)
		}
		key := layout.GenerationKey(network, gen)
		if err := st.Put(ctx, key, data, "application/octet-stream"); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		m.Networks[network] = snapshot.NetworkSnapshot{GenerationKey: key, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}
	}
	if err := snapshot.Write(ctx, st, layout, m); err != nil {
		t.Fatal(err)
	}
	eng, err := query.New(st, networks, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	return eng, st
}

func TestForksEndpoint(t *testing.T) {
	networks := []string{"mainnet", "sepolia"}
	eng, st := forksTestEngine(t, networks)
	layout := snapshot.Layout{}
	now := time.Now()
	tracked := map[string]bool{}
	for _, network := range networks {
		target, err := netconf.ForkTargetAt(network, now)
		if err != nil {
			t.Fatal(err)
		}
		if target.Name == "" {
			continue
		}
		tracked[network] = true
		h := &snapshot.ReadinessHistory{Version: snapshot.ReadinessHistoryVersion, Network: network, Fork: target.Name,
			ELTime: elTimeOf(target), CLEpoch: clEpochOf(target),
			Points: []snapshot.ReadinessPoint{{At: now.Unix(), EL: map[string]int{"ready": 1}}}}
		data, err := h.Encode()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Put(context.Background(), layout.ReadinessHistoryKey(network, target.Name), data, "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	h := routes(eng, "", time.Hour, networks)
	get := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr
	}
	for _, path := range []string{"/api/v1/forks", "/api/v1/forks?network=bogus"} {
		if rr := get(path); rr.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, rr.Code)
		}
	}
	for _, network := range networks {
		rr := get("/api/v1/forks?network=" + network)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", network, rr.Code, rr.Body.String())
		}
		var body query.ForkReadiness
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		target, err := netconf.ForkTargetAt(network, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		want := target.Phase()
		if want == "" {
			want = "none"
		}
		if body.Network != network || body.Phase != want || body.Fork.Name != target.Name {
			t.Errorf("%s: network %q phase %q fork %q, want phase %q fork %q", network, body.Network, body.Phase, body.Fork.Name, want, target.Name)
		}
		if want != "none" && (body.Layers["el"] == nil || body.Layers["el"].Total != 1) {
			t.Errorf("%s: el layer = %+v, want the one seeded row", network, body.Layers["el"])
		}
		if tracked[network] && (body.History == nil || len(body.History.Points) != 1) {
			t.Errorf("%s: history = %+v, want the stored point", network, body.History)
		}
		if rr.Header().Get("Cache-Control") == "" {
			t.Errorf("%s: no Cache-Control", network)
		}
	}
}

func TestForksEndpointSurvivesCorruptHistory(t *testing.T) {
	networks := []string{"sepolia"}
	eng, st := forksTestEngine(t, networks)
	target, err := netconf.ForkTargetAt("sepolia", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if target.Name == "" {
		t.Skip("no fork tracked on sepolia at this date")
	}
	if err := st.Put(context.Background(), snapshot.Layout{}.ReadinessHistoryKey("sepolia", target.Name), []byte("{"), "application/json"); err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	routes(eng, "", time.Hour, networks).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/forks?network=sepolia", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with live counts", rr.Code)
	}
	var body query.ForkReadiness
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body.History != nil || body.Layers["el"] == nil {
		t.Fatalf("body = %+v, %v", body, err)
	}
}

func TestNodeFilterClientExact(t *testing.T) {
	known := map[string]bool{"mainnet": true}
	f, err := nodeFilter(url.Values{"client": {"Geth"}, "client_exact": {"yes"}}, known)
	if err != nil || !f.ClientExact {
		t.Fatalf("client_exact=yes = %+v, %v", f, err)
	}
	if _, err := nodeFilter(url.Values{"client_exact": {"true"}}, known); err == nil {
		t.Fatal("client_exact=true accepted")
	}
}

func elTimeOf(t netconf.ForkTarget) uint64 {
	if t.EL == nil {
		return 0
	}
	return t.EL.Time
}

func clEpochOf(t netconf.ForkTarget) uint64 {
	if t.CL == nil {
		return 0
	}
	return t.CL.Epoch
}
