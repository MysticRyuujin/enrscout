package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
	"github.com/MysticRyuujin/enrscout/internal/query"
	"github.com/MysticRyuujin/enrscout/internal/snapshot"
	"github.com/MysticRyuujin/enrscout/internal/store"
)

func TestNodeRouteResolvesAnEncodedEnodeKey(t *testing.T) {
	ctx := context.Background()
	st, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	id := enode.PubkeyToIDV4(&key.PublicKey).String()
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	gen := time.Now().Truncate(time.Second)
	fork := nw.CurrentForkIDAt(gen)
	data, err := nodeset.ParquetFromRows([]nodeset.Row{{
		ID: id, Network: "mainnet", Layer: "el", IP: "9.9.9.9", TCP: 30303,
		ForkHash: hex.EncodeToString(fork.Hash[:]), ForkNext: fork.Next,
	}})
	if err != nil {
		t.Fatal(err)
	}
	layout := snapshot.Layout{}
	genKey := layout.GenerationKey("mainnet", gen)
	if err := st.Put(ctx, genKey, data, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := snapshot.Write(ctx, st, layout, &snapshot.Manifest{
		SchemaVersion: snapshot.SchemaVersion, GeneratedAt: gen, CrawlerID: "test-crawler",
		Run: snapshot.RunMetadata{
			RunID: "test-run", SourceRevision: "test-revision", SourceURL: "https://example.com/source",
			ConfigSHA256: hex.EncodeToString(make([]byte, sha256.Size)), CrawlerStartedAt: gen.Add(-time.Minute),
			MethodologyStartedAt: gen.Add(-time.Minute), MethodologyVersion: snapshot.MethodologyVersion, MethodologyID: "test-method",
		},
		Networks: map[string]snapshot.NetworkSnapshot{
			"mainnet": {GenerationKey: genKey, NodeCount: 1, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])},
		},
	}); err != nil {
		t.Fatal(err)
	}
	eng, err := query.New(st, []string{"mainnet"}, t.TempDir(), "snapshots")
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.Refresh(ctx); err != nil {
		t.Fatal(err)
	}

	moved := enode.NewV4(&key.PublicKey, net.IP{9, 9, 9, 10}, 30304, 30304).URLv4()
	encoded := strings.NewReplacer(":", "%3A", "/", "%2F", "@", "%40").Replace(moved)
	rr := httptest.NewRecorder()
	routes(eng, "", time.Hour, []string{"mainnet"}).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/nodes/"+encoded, nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), id) {
		t.Fatalf("GET /api/v1/nodes/<encoded enode> = %d %s, want the stored node", rr.Code, rr.Body.String())
	}
}
