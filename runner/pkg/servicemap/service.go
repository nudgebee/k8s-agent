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
	StepSeconds int    // default 3600 (1h step)
	MaxParallel int    // default 8 — caps concurrent /api/v1/query_range requests
}

// New returns a service. Pass nil for prom to disable; handlers will reject.
func New(prom *prometheus.Client, clusterName string) *Service {
	return &Service{
		Prom:        prom,
		ClusterName: clusterName,
		StepSeconds: 3600,
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

	step := s.StepSeconds
	if step <= 0 {
		step = 3600
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

	rangeStep := fmt.Sprintf("%ds", step)
	stepStr := fmt.Sprintf("%ds", step)
	startStr := fmt.Sprintf("%d", start.Unix())
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
			sem <- struct{}{}
			defer func() { <-sem }()

			expanded := expandPlaceholders(q, rangeStep, srcFilter, dstFilter, podFilter, nsFilter, clusterFilter)
			raw, err := s.Prom.QueryRange(ctx, expanded, startStr, endStr, stepStr, "")
			if err != nil {
				resultsCh <- fetchResult{key: key, err: err}
				return
			}
			parsed, err := parsePromRangeResponse(raw)
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
