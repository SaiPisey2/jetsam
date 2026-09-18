//go:build integration

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/corpus"
	"github.com/SaiPisey2/jetsam/internal/inventory"
	"github.com/SaiPisey2/jetsam/internal/promapi"
	"github.com/SaiPisey2/jetsam/internal/verdict"
)

const demoURL = "https://prometheus.demo.prometheus.io"

func TestAgainstThePublicDemo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cl := promapi.New(demoURL, 60*time.Second)

	status, err := cl.TSDBStatus(ctx, 5000)
	if err != nil {
		t.Fatalf("TSDBStatus against the real demo: %v", err)
	}
	if len(status.Counts) < 100 {
		t.Fatalf("got %d metric names, want >=100: the limit parameter is probably not being sent", len(status.Counts))
	}

	rules, err := cl.AlertingAndRecordingRules(ctx)
	if err != nil {
		t.Fatalf("AlertingAndRecordingRules: %v", err)
	}
	if len(rules) < 10 {
		t.Fatalf("got %d rules, want >=10 from the node-exporter and Prometheus mixins", len(rules))
	}

	inv := inventory.Build(status)
	names := make([]string, 0, len(inv.Metrics))
	for _, m := range inv.Metrics {
		names = append(names, m.Name)
	}
	cor := corpus.Build(corpus.Sources{Rules: rules}, names)

	// Real mixin rules must parse. A blocked corpus here means Extract
	// cannot read production PromQL, which is the whole product.
	if len(cor.Blocked) > 0 {
		t.Errorf("real mixin rules failed to parse: %v", cor.Blocked)
	}
	if len(cor.Used) == 0 {
		t.Error("no metric marked used by real mixin rules -- reference extraction is broken")
	}

	res := verdict.Compute(inv, cor)
	if res.DroppableSeries != 0 {
		t.Errorf("DroppableSeries = %d without a query log, want 0", res.DroppableSeries)
	}
	if len(res.Verdicts) != len(inv.Metrics) {
		t.Errorf("graded %d metrics, want all %d", len(res.Verdicts), len(inv.Metrics))
	}
}
