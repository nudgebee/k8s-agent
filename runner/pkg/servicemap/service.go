package servicemap

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// Service orchestrates the parallel Prometheus fetches + world build +
// rendering. Holds a Prometheus client; wired by main.
type Service struct {
	Prom        *prometheus.Client
	ClusterName string // optional; used in __CLUSTER__ filter expansion
	MaxParallel int    // default 8 — caps concurrent /api/v1/query requests

	// CH and Source switch the eBPF edge and container metrics to ClickHouse
	// (see UseClickHouse). With Source empty or SourcePrometheus, or CH nil,
	// Build is PromQL only, as before.
	CH     *chclient.Client
	Source Source
	Logger *slog.Logger

	rollupReady atomic.Bool
	shadowMu    sync.Mutex
	lastShadow  map[string]time.Time
}

// Source is where Build reads the eBPF edge and container metrics from.
type Source string

const (
	// SourcePrometheus reads everything with PromQL (the default).
	SourcePrometheus Source = "prometheus"
	// SourceClickHouse reads edges and container health from ClickHouse and
	// falls back to Prometheus when ClickHouse cannot serve the window.
	SourceClickHouse Source = "clickhouse"
	// SourceShadow serves the PromQL map and, at most once per shadowInterval
	// per window and filter, also builds the ClickHouse map and logs how the
	// two differ.
	SourceShadow Source = "shadow"
)

// ParseSource maps the SERVICE_MAP_SOURCE value to a Source; anything it does
// not recognise is reported as not ok so the caller can warn and keep the
// default.
func ParseSource(v string) (Source, bool) {
	switch s := Source(strings.ToLower(strings.TrimSpace(v))); s {
	case "", SourcePrometheus:
		return SourcePrometheus, true
	case SourceClickHouse, SourceShadow:
		return s, true
	}
	return SourcePrometheus, false
}

// shadowInterval caps how often shadow mode re-runs the ClickHouse build for
// the same window and filter: SLO and anomaly checks call service_map often,
// and each comparison is a second full build.
const shadowInterval = 15 * time.Minute

// rollupRetryInterval is how often UseClickHouse retries creating the rollup
// while the collector has not created the raw tables yet.
const rollupRetryInterval = time.Minute

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

	// ClickHouse is read over the same window the PromQL $RANGE spans.
	start = end.Add(-window)
	fetch := func(keys map[string]string) map[string][]promResult {
		return s.fetchProm(ctx, keys, endStr, func(q string) string {
			return expandPlaceholders(q, rangeStr, srcFilter, dstFilter, podFilter, nsFilter, clusterFilter)
		})
	}

	switch s.source() {
	case SourceClickHouse:
		chMetrics, err := fetchClickHouse(ctx, s.CH, start, end, p)
		if err == nil {
			// Pods, services and replicas still come from kube-state-metrics.
			metrics := fetch(kubeQueries(queryList))
			maps.Copy(metrics, chMetrics)
			return render(build(metrics)), nil
		}
		s.logger().Warn("service map: clickhouse cannot serve this window, using prometheus",
			"reason", err, "window", window.String())
	case SourceShadow:
		metrics := fetch(queryList)
		s.maybeShadow(start, end, window, p, metrics)
		return render(build(metrics)), nil
	}
	return render(build(fetch(queryList))), nil
}

// fetchProm runs the given queries in parallel, at most MaxParallel at a time.
// A query that fails is left out rather than failing the whole map.
func (s *Service) fetchProm(ctx context.Context, queryList map[string]string, endStr string, expand func(string) string) map[string][]promResult {
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

			raw, err := s.Prom.Query(ctx, expand(q), endStr, "")
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
	return metrics
}

// kubeQueries is the kube-state-metrics part of a query list: the queries the
// ClickHouse source does not replace.
func kubeQueries(queryList map[string]string) map[string]string {
	out := map[string]string{}
	for k, q := range queryList {
		if strings.HasPrefix(k, "kube_") {
			out[k] = q
		}
	}
	return out
}

func (s *Service) source() Source {
	if s.CH == nil {
		return SourcePrometheus
	}
	switch s.Source {
	case SourceClickHouse, SourceShadow:
		return s.Source
	}
	return SourcePrometheus
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// UseClickHouse attaches the ClickHouse client and source, and — unless the
// source is prometheus — keeps trying to create the per-minute rollup until
// the collector has created the tables it reads. Nothing is created in
// ClickHouse with the default source. Call it once, at startup.
func (s *Service) UseClickHouse(ctx context.Context, c *chclient.Client, src Source, logger *slog.Logger) {
	s.CH, s.Source, s.Logger = c, src, logger
	if s.source() == SourcePrometheus {
		return
	}
	go func() {
		t := time.NewTicker(rollupRetryInterval)
		defer t.Stop()
		for {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			ok, err := ensureRollup(cctx, c)
			cancel()
			switch {
			case ok:
				s.rollupReady.Store(true)
				s.logger().Info("service map: clickhouse rollup ready", "table", rollupTable, "source", string(s.Source))
				return
			case err != nil:
				s.logger().Warn("service map: cannot create clickhouse rollup yet", "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// maybeShadow builds the ClickHouse map for the same window in the
// background and logs how it differs from the PromQL map just served. It runs
// at most once per shadowInterval per window and filter, and never delays or
// changes the response.
func (s *Service) maybeShadow(start, end time.Time, window time.Duration, p FilterParams, promMetrics map[string][]promResult) {
	key := fmt.Sprintf("%d|%s|%s", int64(window.Seconds()), p.WorkloadNamespace, p.WorkloadName)
	now := time.Now()
	s.shadowMu.Lock()
	if s.lastShadow == nil {
		s.lastShadow = map[string]time.Time{}
	}
	if last, ok := s.lastShadow[key]; ok && now.Sub(last) < shadowInterval {
		s.shadowMu.Unlock()
		return
	}
	s.lastShadow[key] = now
	s.shadowMu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		t0 := time.Now()
		chMetrics, err := fetchClickHouse(ctx, s.CH, start, end, p)
		chTook := time.Since(t0)
		if err != nil {
			s.logger().Info("service map shadow: clickhouse build skipped", "reason", err, "window", window.String())
			return
		}
		merged := map[string][]promResult{}
		for k, v := range promMetrics {
			if strings.HasPrefix(k, "kube_") {
				merged[k] = v
			}
		}
		maps.Copy(merged, chMetrics)
		d := compareMaps(render(build(promMetrics)), render(build(merged)))
		s.logger().Info("service map shadow: prometheus vs clickhouse",
			"window", window.String(),
			"workload_namespace", p.WorkloadNamespace, "workload_name", p.WorkloadName,
			"links_prometheus", d.prom, "links_clickhouse", d.ch, "links_both", d.both,
			"only_prometheus", d.onlyProm, "only_clickhouse", d.onlyCH,
			"requests_within_5pct", d.reqWithin5, "requests_compared", d.reqCompared,
			"clickhouse_fetch_ms", chTook.Milliseconds(),
			// ClickHouse rates average over the whole window; PromQL rate() over
			// each series' lifetime, so short-lived pods read higher there.
			// ClickHouse latency is the edge mean, PromQL the slowest series' mean.
			"note", "rates are window averages; latency is the edge mean")
	}()
}

type mapDiff struct {
	prom, ch, both, onlyProm, onlyCH, reqWithin5, reqCompared int
}

// compareMaps compares two rendered maps link by link (source app, upstream,
// protocol) and counts links whose request rates are within 5%.
func compareMaps(prom, ch []Application) mapDiff {
	links := func(apps []Application) map[string]float64 {
		out := map[string]float64{}
		for _, a := range apps {
			for _, u := range a.Upstreams {
				out[idToText(a.ID)+" -> "+u.ID+" ["+u.Protocol+"]"] = u.RequestCount
			}
		}
		return out
	}
	pl, cl := links(prom), links(ch)
	d := mapDiff{prom: len(pl), ch: len(cl)}
	for k, pv := range pl {
		cv, ok := cl[k]
		if !ok {
			d.onlyProm++
			continue
		}
		d.both++
		if pv == 0 && cv == 0 {
			continue
		}
		d.reqCompared++
		hi := pv
		if cv > hi {
			hi = cv
		}
		diff := pv - cv
		if diff < 0 {
			diff = -diff
		}
		if diff/hi <= 0.05 {
			d.reqWithin5++
		}
	}
	d.onlyCH = len(cl) - d.both
	return d
}
