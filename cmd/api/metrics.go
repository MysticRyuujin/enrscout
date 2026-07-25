package main

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/MysticRyuujin/enrscout/internal/query"
)

var (
	apiRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_api_requests_total",
		Help: "API requests by route and status code.",
	}, []string{"route", "code"})
	apiDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "enrscout_api_request_duration_seconds",
		Help:    "API request duration by route.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route"})
	apiRefreshTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "enrscout_api_refresh_total",
		Help: "Snapshot refresh attempts by result.",
	}, []string{"result"})
	apiLoadedNodes = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_api_loaded_nodes",
		Help: "Nodes currently loaded in the query engine.",
	})
	apiSnapshotGenerated = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_api_snapshot_generated_timestamp_seconds",
		Help: "generated_at of the loaded snapshot manifest.",
	})
	apiLastRefresh = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "enrscout_api_last_refresh_timestamp_seconds",
		Help: "Time of the last successful snapshot refresh.",
	})
)

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

func instrument(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next(rec, r)
		apiDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
		apiRequests.WithLabelValues(route, strconv.Itoa(rec.code)).Inc()
	}
}

func recordRefresh(eng *query.Engine, err error) {
	if err != nil {
		apiRefreshTotal.WithLabelValues("failure").Inc()
		return
	}
	apiRefreshTotal.WithLabelValues("success").Inc()
	apiLoadedNodes.Set(float64(eng.Loaded()))
	apiLastRefresh.SetToCurrentTime()
	if g := eng.GeneratedAt(); !g.IsZero() {
		apiSnapshotGenerated.Set(float64(g.Unix()))
	}
}
