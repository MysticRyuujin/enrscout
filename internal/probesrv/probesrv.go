package probesrv

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/p2p/enode"

	"github.com/MysticRyuujin/enrscout/internal/enrich"
	"github.com/MysticRyuujin/enrscout/internal/netconf"
	"github.com/MysticRyuujin/enrscout/internal/nodeset"
)

const maxBody = 8 << 10

type Options struct {
	Addr                 string
	Token                string
	AllowUnauthenticated bool
}

type result struct {
	ID         string `json:"id"`
	Layer      string `json:"layer"`
	Network    string `json:"network,omitempty"`
	Client     string `json:"client,omitempty"`
	Version    string `json:"client_version,omitempty"`
	OS         string `json:"os,omitempty"`
	Lang       string `json:"lang,omitempty"`
	Caps       string `json:"capabilities,omitempty"`
	Registered bool   `json:"registered"`
	Error      string `json:"error,omitempty"`
}

// Server is the running probe endpoint. Shutdown stops accepting and waits for in-flight probes,
// which matters because a handler writes to the shared nodeset: a caller taking a final snapshot
// needs those writes to have landed or not started.
type Server struct {
	srv  *http.Server
	addr string

	// A handler writes to the nodeset, so Shutdown must know when the last one is done. Adds are
	// guarded by mu rather than merely ordered after the listener closes: http.Server.Shutdown can
	// return on deadline while a request is still being dispatched, which would race active.Wait.
	mu      sync.RWMutex
	closing bool
	active  sync.WaitGroup
}

// enter reserves a handler slot, reporting false once shutdown has begun.
func (s *Server) enter() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closing {
		return false
	}
	s.active.Add(1)
	return true
}

// Addr is the resolved listen address, which differs from Options.Addr when port 0 was requested.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Shutdown stops accepting probes and returns only once the handlers already running have
// finished, so their nodeset writes cannot land after a caller's final snapshot. A graceful
// http.Server.Shutdown that exceeds ctx is reported but not obeyed: abandoning the wait is the
// very thing that would let a late write through. Safe on a nil Server, which is what Start
// returns when the endpoint is disabled.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil || s.srv == nil {
		return nil
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	err := s.srv.Shutdown(ctx)
	s.active.Wait()
	return err
}

// Start serves /probe (empty Addr = off). The endpoint can dial attacker-selected
// addresses, so it requires bearer authentication unless the operator makes the
// unsafe local-development exception explicit.
func Start(opts Options, fp *enrich.Fingerprinter, clfp *enrich.CLFingerprinter, geo *enrich.Geo, set *nodeset.Set, timeout time.Duration, allowDevnet bool) (*Server, error) {
	if opts.Addr == "" {
		return nil, nil
	}
	if opts.Token == "" && !opts.AllowUnauthenticated {
		return nil, errors.New("probe server requires a token or explicit unauthenticated opt-in")
	}
	tokenHash := sha256.Sum256([]byte(opts.Token))
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /probe", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if opts.Token != "" && !authorized(r.Header.Get("Authorization"), tokenHash) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="enrscout-probe"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !s.enter() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		defer s.active.Done()
		handle(w, r, fp, clfp, geo, set, timeout, allowDevnet)
	})
	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen probe %s: %w", opts.Addr, err)
	}
	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       timeout + 5*time.Second,
		WriteTimeout:      timeout + 5*time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		slog.Info("probe server listening", "addr", opts.Addr, "authenticated", opts.Token != "")
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("probe server stopped", "err", err)
		}
	}()
	s.srv, s.addr = srv, ln.Addr().String()
	return s, nil
}

func authorized(header string, tokenHash [sha256.Size]byte) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	gotHash := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(gotHash[:], tokenHash[:]) == 1
}

func handle(w http.ResponseWriter, r *http.Request, fp *enrich.Fingerprinter, clfp *enrich.CLFingerprinter, geo *enrich.Geo, set *nodeset.Set, timeout time.Duration, allowDevnet bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		http.Error(w, "read request", http.StatusBadRequest)
		return
	}
	if len(b) > maxBody {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	raw := strings.TrimSpace(string(b))
	if raw == "" {
		http.Error(w, "missing enr", http.StatusBadRequest)
		return
	}
	n, err := parse(raw)
	if err != nil {
		http.Error(w, "invalid record: "+err.Error(), http.StatusBadRequest)
		return
	}

	forceDevnet := strings.TrimSpace(r.URL.Query().Get("network")) == "devnet"
	layer, network, routable, err := classify(n, forceDevnet, allowDevnet)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if !routable {
		http.Error(w, "record advertises no globally-routable ip", http.StatusUnprocessableEntity)
		return
	}
	if layer != "el" && layer != "cl" {
		http.Error(w, "unclassified record (no eth or eth2 ENR entry)", http.StatusUnprocessableEntity)
		return
	}
	if (layer == "el" && fp == nil) || (layer == "cl" && clfp == nil) {
		http.Error(w, layer+" fingerprinting disabled", http.StatusServiceUnavailable)
		return
	}

	res := result{ID: n.ID().String(), Layer: layer, Network: network}
	var observed nodeset.Observation
	if set != nil {
		if forceDevnet {
			observed = set.ObserveSeedResult(n, "devnet", time.Now())
		} else {
			observed = set.ObserveResult(n, "probe", time.Now())
		}
	}
	if observed.Accepted {
		res.Registered = true
		if observed.Changed && geo != nil {
			addr := n.IP()
			g := geo.Lookup(addr)
			set.SetGeo(n.ID(), addr, g.Country, g.City, g.Subdivision, g.Lat, g.Lon, g.ASN, g.Org, g.Hosting, g.HostingKnown, g.Geolocated, g.AccuracyRadiusKM)
		}
	}
	claimed := observed.Applied && set.ClaimFingerprint(n.ID())

	ctx, cancel := context.WithTimeout(r.Context(), timeout+time.Second)
	defer cancel()

	var fpr enrich.Fingerprint
	if layer == "el" {
		if forceDevnet {
			nw, getErr := netconf.Get("devnet")
			if getErr != nil {
				err = getErr
			} else {
				fpr, err = fp.ProbeStatus(ctx, n, nw)
			}
		} else {
			fpr, err = fp.Probe(ctx, n)
		}
	} else {
		fpr, err = clfp.Probe(ctx, n)
	}
	if err != nil {
		res.Error = err.Error()
	} else {
		res.Client, res.Version, res.OS, res.Lang, res.Caps = fpr.Client, fpr.Version, fpr.OS, fpr.Lang, fpr.Caps
	}
	// Status is gated on the fingerprint completion: a claimed probe whose record
	// changed mid-probe is discarded, and a probe of a stale record (accepted but
	// not applied) must not overwrite the newer retained record's network either.
	if finishRegisteredProbe(set, n.ID(), res.Registered, claimed, observed.Applied, fpr, err) && layer == "el" && fpr.Network != "" {
		set.SetExecutionStatus(n.ID(), fpr.Network, fpr.ForkID)
		res.Network = fpr.Network
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func finishRegisteredProbe(set *nodeset.Set, id enode.ID, registered, claimed, applied bool, fp enrich.Fingerprint, probeErr error) bool {
	if !registered || set == nil {
		return false
	}
	if probeErr != nil {
		if claimed {
			set.FingerprintFailed(id, time.Now())
		}
		return false
	}
	if claimed {
		_, ok := set.SetClaimedFingerprint(id, fp.Client, fp.Version, fp.OS, fp.Lang, fp.Caps, "outbound")
		return ok
	}
	set.SetFingerprint(id, fp.Client, fp.Version, fp.OS, fp.Lang, fp.Caps, "outbound")
	return applied
}

func classify(n *enode.Node, forceDevnet, allowDevnet bool) (layer, network string, routable bool, err error) {
	layer, network, routable = nodeset.Inspect(n)
	if !forceDevnet {
		return layer, network, routable, nil
	}
	if !allowDevnet {
		return "", "", false, errors.New("devnet override requires --devnet-only")
	}
	layer, network, routable = nodeset.InspectDevnet(n)
	if layer != "el" && layer != "cl" {
		return layer, network, routable, errors.New("devnet override requires an EL TCP endpoint or a classifiable CL record")
	}
	return layer, "devnet", routable, nil
}

func parse(s string) (*enode.Node, error) {
	if n, err := enode.Parse(enode.ValidSchemes, s); err == nil {
		return n, nil
	}
	return enode.ParseV4(s)
}
