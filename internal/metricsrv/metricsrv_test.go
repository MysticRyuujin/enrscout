package metricsrv

import (
	"sort"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestServiceLabeled(t *testing.T) {
	reg := prometheus.NewRegistry()
	plain := prometheus.NewCounter(prometheus.CounterOpts{Name: "plain_total"})
	labeled := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "labeled_total"}, []string{"service", "zz"})
	reg.MustRegister(plain, labeled)
	plain.Inc()
	labeled.WithLabelValues("preset", "v").Inc()

	mfs, err := serviceLabeled(reg, "crawler").Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			if !sort.SliceIsSorted(m.GetLabel(), func(i, j int) bool {
				return m.GetLabel()[i].GetName() < m.GetLabel()[j].GetName()
			}) {
				t.Errorf("%s: labels not sorted", mf.GetName())
			}
			var got string
			for _, l := range m.GetLabel() {
				if l.GetName() == "service" {
					got = l.GetValue()
				}
			}
			want := "crawler"
			if mf.GetName() == "labeled_total" {
				want = "preset"
			}
			if got != want {
				t.Errorf("%s: service label = %q, want %q", mf.GetName(), got, want)
			}
		}
	}
}
