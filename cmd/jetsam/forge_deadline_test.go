package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SaiPisey2/jetsam/internal/forge"
)

// capturingProvider wraps a forge.Provider and records the context each
// method receives, so a test can inspect its deadline directly rather than
// inferring it from timing.
type capturingProvider struct {
	forge.Provider
	ctxs []context.Context
}

func (c *capturingProvider) FindPR(ctx context.Context, owner, repo, base, branch string) (*forge.PullRequest, error) {
	c.ctxs = append(c.ctxs, ctx)
	return c.Provider.FindPR(ctx, owner, repo, base, branch)
}

func (c *capturingProvider) EnsureBranch(ctx context.Context, owner, repo, branch, base string) error {
	c.ctxs = append(c.ctxs, ctx)
	return c.Provider.EnsureBranch(ctx, owner, repo, branch, base)
}

func (c *capturingProvider) CommitFiles(ctx context.Context, owner, repo, base, branch string, changes []forge.FileChange) error {
	c.ctxs = append(c.ctxs, ctx)
	return c.Provider.CommitFiles(ctx, owner, repo, base, branch, changes)
}

func (c *capturingProvider) OpenPR(ctx context.Context, spec forge.PRSpec) (*forge.PullRequest, error) {
	c.ctxs = append(c.ctxs, ctx)
	return c.Provider.OpenPR(ctx, spec)
}

// TestProposeApplyGivesTheForgePhaseItsOwnLongerDeadline pins I4: the forge
// phase (EnsureBranch/CommitFiles/OpenPR) must run under a context that is
// NOT derived from cfg.Prometheus.Timeout. This configures a deliberately
// tiny Prometheus timeout -- long enough for the in-process fake Prometheus
// to answer, far too short to survive to the forge calls if they shared it
// -- and asserts every context the forge saw still has minutes of budget
// left, proving the two phases are decoupled.
func TestProposeApplyGivesTheForgePhaseItsOwnLongerDeadline(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status/tsdb":
			w.Write([]byte(`{"status":"success","data":{"seriesCountByMetricName":[
				{"name":"orphan_metric_total","value":123}]}}`))
		case "/api/v1/rules":
			w.Write([]byte(`{"status":"success","data":{"groups":[]}}`))
		case "/api/v1/query":
			w.Write([]byte(`{"status":"success","data":{"result":[{"metric":{"job":"api"}}]}}`))
		default:
			w.Write([]byte(`{"status":"success","data":{"result":[]}}`))
		}
	}))
	defer prom.Close()

	dir := t.TempDir()
	promYAMLPath := filepath.Join(dir, "prometheus.yml")
	os.WriteFile(promYAMLPath, []byte("global:\n  scrape_interval: 15s\nscrape_configs:\n  - job_name: api\n    static_configs:\n      - targets: ['localhost:9100']\n"), 0o644)

	cfgPath := filepath.Join(dir, "jetsam.yaml")
	// A 5-second Prometheus timeout is ample for the in-process fake server
	// above, and far short of the 5-minute forge deadline this fix wires
	// in -- if the forge phase ever went back to sharing cfg.Prometheus's
	// context, this test's captured deadlines would collapse toward
	// "5 seconds from process start", not "5 minutes from just now".
	os.WriteFile(cfgPath, []byte("prometheus:\n  url: "+prom.URL+"\n  timeout: 5s\nprometheus_file: "+promYAMLPath+"\n"), 0o600)

	cp := &capturingProvider{Provider: forge.NewFakeProvider()}
	newProvider := func(token string) forge.Provider { return cp }

	var stdout, stderr bytes.Buffer
	rc := proposeCmd([]string{"-config", cfgPath, "-include-unreferenced", "-apply", "-owner", "o", "-repo", "r"},
		&stdout, &stderr, sentinelEnv, newProvider)
	if rc != 0 {
		t.Fatalf("proposeCmd = %d, want 0: stderr=%s", rc, stderr.String())
	}
	if len(cp.ctxs) == 0 {
		t.Fatal("no forge method was ever called; nothing to assert about its context")
	}
	for i, ctx := range cp.ctxs {
		dl, ok := ctx.Deadline()
		if !ok {
			t.Errorf("forge call %d: context has no deadline at all", i)
			continue
		}
		if remaining := time.Until(dl); remaining < 4*time.Minute {
			t.Errorf("forge call %d: only %v left on its context's deadline, want most of a fresh 5m budget "+
				"(a Prometheus-timeout-sized context would have far less)", i, remaining)
		}
	}
}
