package version

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAccessorsReturnLinkerValues(t *testing.T) {
	if Version() == "" || Commit() == "" || Date() == "" {
		t.Fatalf("version metadata must never be empty, got %q %q %q", Version(), Commit(), Date())
	}
}

func TestBuildInfoIsAlwaysOne(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(Collector())

	got := testutil.CollectAndCount(Collector())
	if got != 1 {
		t.Fatalf("build_info should export exactly one series, got %d", got)
	}

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "sandman_build_info" {
		t.Fatalf("expected a single sandman_build_info family, got %v", families)
	}
	metric := families[0].GetMetric()[0]
	if metric.GetGauge().GetValue() != 1 {
		t.Fatalf("build_info must be 1, got %v", metric.GetGauge().GetValue())
	}
	labels := make([]string, 0, len(metric.GetLabel()))
	for _, l := range metric.GetLabel() {
		labels = append(labels, l.GetName())
	}
	if want := "commit,date,goversion,version"; strings.Join(labels, ",") != want {
		t.Fatalf("labels = %q, want %q", strings.Join(labels, ","), want)
	}
}
