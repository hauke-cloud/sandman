package power

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// prometheusRegistry returns a registry scoped to one test, so instruments
// registered by one test cannot collide with another's.
func prometheusRegistry(t *testing.T) prometheus.Registerer {
	t.Helper()
	return prometheus.NewRegistry()
}
