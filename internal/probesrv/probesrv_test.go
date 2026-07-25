package probesrv

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

func TestClassifyDevnetTCPEnode(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := enode.NewV4(&key.PublicKey, net.ParseIP("1.2.3.4"), 30303, 30303)

	layer, network, routable, err := classify(n, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if layer != "el" || network != "devnet" || !routable {
		t.Fatalf("got layer=%q network=%q routable=%v", layer, network, routable)
	}
}

func TestClassifyDevnetTCP6Only(t *testing.T) {
	var r enr.Record
	r.Set(enr.IPv6{0x26, 0x06, 0x47, 0x00, 0x47, 0x00, 0, 0, 0, 0, 0, 0, 0, 0, 0x11, 0x11})
	r.Set(enr.TCP6(30303))
	var id enode.ID
	id[0] = 78
	layer, network, routable, err := classify(enode.SignNull(&r, id), true, true)
	if err != nil {
		t.Fatal(err)
	}
	if layer != "el" || network != "devnet" || !routable {
		t.Fatalf("got layer=%q network=%q routable=%v", layer, network, routable)
	}
}

func TestBearerAuthorization(t *testing.T) {
	tokenHash := sha256.Sum256([]byte("correct horse"))
	if !authorized("Bearer correct horse", tokenHash) {
		t.Fatal("valid bearer token rejected")
	}
	for _, header := range []string{"", "Basic correct horse", "Bearer wrong", "Bearer correct horse extra", "Bearer correct hors"} {
		if authorized(header, tokenHash) {
			t.Errorf("invalid authorization accepted: %q", header)
		}
	}
}

func TestProbeRejectsOversizedBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/probe", strings.NewReader(strings.Repeat("x", maxBody+1)))
	w := httptest.NewRecorder()
	handle(w, r, nil, nil, nil, nil, time.Second, false)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestOnDemandFailureDoesNotReleaseCrawlerClaim(t *testing.T) {
	nw, err := netconf.Get("mainnet")
	if err != nil {
		t.Fatal(err)
	}
	var r enr.Record
	r.Set(enr.IPv4{1, 2, 3, 4})
	r.Set(enr.TCP(30303))
	r.Set(netconf.EthEntry{ForkID: nw.CurrentForkID()})
	var id enode.ID
	id[0] = 77
	n := enode.SignNull(&r, id)
	set := nodeset.NewWithLimit(0)
	if !set.Observe(n, "v5", time.Now()) || !set.ClaimFingerprint(id) {
		t.Fatal("crawler claim setup failed")
	}
	finishRegisteredProbe(set, id, true, false, true, enrich.Fingerprint{}, errors.New("probe failed"))
	if set.ClaimFingerprint(id) {
		t.Fatal("on-demand failure released a fingerprint claim owned by the crawler")
	}
}

func TestClassifyDevnetOverrideDisabled(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	n := enode.NewV4(&key.PublicKey, net.ParseIP("1.2.3.4"), 30303, 30303)
	if _, _, _, err := classify(n, true, false); err == nil {
		t.Fatal("expected disabled devnet override to fail")
	}
}

// A disabled endpoint yields a nil *Server, and shutdown paths call Shutdown unconditionally.
func TestShutdownOnDisabledServerIsSafe(t *testing.T) {
	srv, err := Start(Options{}, nil, nil, nil, nil, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if srv != nil {
		t.Fatal("disabled endpoint returned a server")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown on a nil server: %v", err)
	}
}

func TestShutdownIsIdempotentAndResolvesAddr(t *testing.T) {
	srv, err := Start(Options{Addr: "127.0.0.1:0", AllowUnauthenticated: true},
		nil, nil, nil, nil, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Addr() == "" || strings.HasSuffix(srv.Addr(), ":0") {
		t.Fatalf("Addr() = %q, want the resolved ephemeral port", srv.Addr())
	}
	probe, err := net.DialTimeout("tcp", srv.Addr(), time.Second)
	if err != nil {
		t.Fatalf("probe endpoint not listening: %v", err)
	}
	probe.Close()
	// Stopping accepts and draining handlers is http.Server.Shutdown's contract; what is ours is
	// that Shutdown is wired up, returns cleanly, and tolerates the repeat call shutdown() makes.
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
}

// Even when graceful shutdown exceeds its deadline, Shutdown must not return while a handler is
// running: abandoning the wait is exactly what lets a nodeset write land after the final snapshot.
func TestShutdownWaitsForHandlersPastDeadline(t *testing.T) {
	srv, err := Start(Options{Addr: "127.0.0.1:0", AllowUnauthenticated: true},
		nil, nil, nil, nil, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool
	srv.srv.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !srv.enter() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		defer srv.active.Done()
		close(entered)
		<-release
		finished.Store(true)
	})

	go func() { http.Post("http://"+srv.Addr()+"/probe", "text/plain", strings.NewReader("x")) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("handler never ran")
	}

	done := make(chan error, 1)
	// A deadline far shorter than the handler, so http.Server.Shutdown gives up first.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	go func() { done <- srv.Shutdown(ctx) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a handler was still running")
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Shutdown err = %v, want the graceful deadline reported", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}
	if !finished.Load() {
		t.Fatal("Shutdown returned before the handler completed")
	}
}
