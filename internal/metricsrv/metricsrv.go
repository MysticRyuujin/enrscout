package metricsrv

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"sort"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

// Start serves the default registry with a constant service label on every
// series. The crawler, api, and dnspublisher all expose the same process_*/go_*
// collectors; without the label, scrapes of two binaries on one host produce
// identical label sets that clobber each other in a shared TSDB.
func Start(addr, service string) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(serviceLabeled(prometheus.DefaultGatherer, service), promhttp.HandlerOpts{}))
	return serve("metrics", addr, mux)
}

// StartPprof serves pprof on addr (empty = off); bind loopback/private, never public.
func StartPprof(addr string) error {
	if addr == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("POST /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return serve("pprof", addr, mux)
}

// serve binds synchronously so a bad production configuration fails at startup.
func serve(name, addr string, h http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s %s: %w", name, addr, err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		slog.Info(name+" server listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error(name+" server stopped", "err", err)
		}
	}()
	return nil
}

func serviceLabeled(g prometheus.Gatherer, service string) prometheus.Gatherer {
	name := "service"
	return prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		mfs, err := g.Gather()
		if err != nil {
			return nil, err
		}
		for _, mf := range mfs {
			for _, m := range mf.GetMetric() {
				if hasLabel(m, name) {
					continue
				}
				m.Label = append(m.Label, &dto.LabelPair{Name: &name, Value: &service})
				sort.Slice(m.Label, func(i, j int) bool {
					return m.Label[i].GetName() < m.Label[j].GetName()
				})
			}
		}
		return mfs, nil
	})
}

func hasLabel(m *dto.Metric, name string) bool {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return true
		}
	}
	return false
}
