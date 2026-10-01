package servicemap

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// Service orchestrates the parallel Prometheus fetches + world build +
// rendering. Holds a Prometheus client; wired by main.
type Service struct {
	Prom        *prometheus.Client
	ClusterName string // optional; used in __CLUSTER__ filter expansion
	MaxParallel int    // default 8 — caps concurrent /api/v1/query requests
}

// minWindow is the shortest window the map is computed over. rate() needs at
// least two samples per series, which a shorter window can miss at common
// scrape intervals; a shorter request is widened to this.
const minWindow = 5 * time.Minute

// New returns a service. Pass nil for prom to disable; handlers will reject.
func New(prom *prometheus.Client, clusterName string) *Service {
	return &Service{
		Prom:        prom,
		ClusterName: clusterName,
		MaxParallel: 8,
	}
}

// Build fetches all queries in QUERIES (or APPLICATION_QUERIES if a filter
// is set), constructs the world, and renders applications. Returns the
// list ready for JSON encoding.
func (s *Service) Build(ctx context.Context, p FilterParams) ([]Application, error) {
	if s.Prom == nil {
		return nil, fmt.Errorf("servicemap: prometheus client not configured")
	}

	end := time.Now().UTC()
	if p.EndTime != "" {
		if t, err := time.Parse(time.RFC3339, p.EndTime); err == nil {
			end = t
		}
	}
	durationMin := p.Duration
	if durationMin <= 0 {
		durationMin = 1440 // 24h
	}
	start := end.Add(-time.Duration(durationMin) * time.Minute)
	if p.StartTime != "" {
		if t, err := time.Parse(time.RFC3339, p.StartTime); err == nil {
			start = t
		}
	}

	// Filter expansion. The pod_filter default is `pod=~".*"` per
	//
	srcFilter := ""
	dstFilter := ""
	podFilter := `pod=~".*"`
	nsFilter := ""
	if p.WorkloadName != "" {
		srcFilter = dictToPrometheusFilter(map[string]string{
			"src_workload_name":      p.WorkloadName,
			"src_workload_namespace": p.WorkloadNamespace,
		})
		dstFilter = dictToPrometheusFilter(map[string]string{
			"destination_workload_name":      p.WorkloadName,
			"destination_workload_namespace": p.WorkloadNamespace,
		})
		podFilter = dictToPrometheusFilter(map[string]string{"pod": p.WorkloadName + "%"})
		nsFilter = dictToPrometheusFilter(map[string]string{"namespace": p.WorkloadNamespace})
	} else if p.WorkloadNamespace != "" {
		srcFilter = dictToPrometheusFilter(map[string]string{"src_workload_namespace": p.WorkloadNamespace})
		dstFilter = dictToPrometheusFilter(map[string]string{"destination_workload_namespace": p.WorkloadNamespace})
		nsFilter = dictToPrometheusFilter(map[string]string{"namespace": p.WorkloadNamespace})
	}
	clusterFilter := ""
	if s.ClusterName != "" {
		clusterFilter = `cluster="` + s.ClusterName + `",`
	}

	queryList := Queries
	if p.WorkloadName != "" || p.WorkloadNamespace != "" {
		queryList = ApplicationQueries
	}

	// Each query is evaluated once, at the end of the window, over the whole
	// window: rates and counts then cover exactly the range the caller
	// selected instead of the last hour-aligned step of it.
	window := end.Sub(start)
	if window < minWindow {
		window = minWindow
	}
	rangeStr := fmt.Sprintf("%ds", int64(window.Seconds()))
	endStr := fmt.Sprintf("%d", end.Unix())

	// Parallel fetch with a bounded worker pool.
	type fetchResult struct {
		key  string
		data []promResult
		err  error
	}
	resultsCh := make(chan fetchResult, len(queryList))
	sem := make(chan struct{}, s.MaxParallel)
	var wg sync.WaitGroup

	for key, q := range queryList {
		key, q := key, q
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Don't queue behind running queries once the caller has given up.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				resultsCh <- fetchResult{key: key, err: ctx.Err()}
				return
			}
			defer func() { <-sem }()

			expanded := expandPlaceholders(q, rangeStr, srcFilter, dstFilter, podFilter, nsFilter, clusterFilter)
			raw, err := s.Prom.Query(ctx, expanded, endStr, "")
			if err != nil {
				resultsCh <- fetchResult{key: key, err: err}
				return
			}
			parsed, err := parsePromResponse(raw)
			resultsCh <- fetchResult{key: key, data: parsed, err: err}
		}()
	}
	wg.Wait()
	close(resultsCh)

	metrics := map[string][]promResult{}
	for r := range resultsCh {
		if r.err != nil {
			// Don't fail the whole map for one bad query — log via the
			// caller. Continue with partial data.
			continue
		}
		metrics[r.key] = r.data
	}

	w := build(metrics)
	return render(w), nil
}
