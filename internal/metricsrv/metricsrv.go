package metricsrv

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen metrics %s: %w", addr, err)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		slog.Info("metrics server listening", "addr", addr)
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server stopped", "err", err)
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
