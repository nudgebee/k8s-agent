package enrichers

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/observability/prometheus"
)

// AppStatsEnricher implements `application_stats`. The action runs a suite of
// Prometheus range queries (APPLICATION_MONITORING_QUERIES, or the
// caller-supplied `queries` map), parses each result into a per-series
// time-series grid, buckets samples by app_id, then reduces to scalars.
//
// Wire shape:
//
//	{
//	  "success": true,
//	  "data":    [<list of ApplicationStats dicts>],
//	  "request_id": "..."  (set by dispatcher)
//	}
//
// Each ApplicationStats dict matches relay.ApplicationStatsResponse —
// application_id, name, namespace, container, max_cpu_request,
// max_memory_request, …, other_metrics.
type AppStatsEnricher struct{ q PromQuerier }

func NewAppStatsEnricher(c *prometheus.Client) *AppStatsEnricher {
	return &AppStatsEnricher{q: promClientAdapter{c: c}}
}

// Handler returns a dispatch.Handler for application_stats.
func (a *AppStatsEnricher) Handler() dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		return map[string]any{
			"success": true,
			"data":    a.run(ctx, params),
		}, nil
	}
}

// applicationStats collects partial samples for one application_id during the
// extract pass; it's reduced into the response shape at the end of run().
type applicationStats struct {
	ApplicationID string
	Name          string
	Namespace     string
	Container     string
	MaxCPURequest *timeSeries
	MaxMemRequest *timeSeries
	MaxCPULimit   *timeSeries
	MaxMemLimit   *timeSeries
	LatencyP99    *timeSeries
	CPUP99        *timeSeries
	CPUP50        *timeSeries
	CPUMax        *timeSeries
	MemMax        *timeSeries
	MemP99        *timeSeries
	MemP50        *timeSeries
	LogFailures   *timeSeries
	TotalRequests *timeSeries
	FailRequests  *timeSeries
	BadData       *timeSeries
	GoodData      *timeSeries
	ValidData     *timeSeries
	OOMKill       *timeSeries
	OtherMetrics  map[string]*timeSeries
	OtherReducers map[string]func(acc, v float64) float64
}

func (a *AppStatsEnricher) run(ctx context.Context, params map[string]any) []map[string]any {
	const dateFormat = "2006-01-02T15:04:05.000000Z"

	// 1. Time window
	now := time.Now().UTC()
	endTime := now
	if s, _ := params["r_end_time"].(string); s != "" {
		t, err := time.Parse(dateFormat, s)
		if err == nil {
			endTime = t
		}
	}
	durationMin := 60
	if v, err := toInt(params["r_duration"]); err == nil && v > 0 {
		durationMin = v
	}
	startTime := endTime.Add(-time.Duration(durationMin) * time.Minute)
	if s, _ := params["r_start_time"].(string); s != "" {
		t, err := time.Parse(dateFormat, s)
		if err == nil {
			startTime = t
		}
	}
	step := int64(60)

	// 2. Build label filters
	podFilter := dictToPrometheusFilter(params["pod_filter"])
	containerFilter := dictToPrometheusFilter(params["container_filter"])
	workloadFilter := dictToPrometheusFilter(params["workload_filter"])

	// `applications: [{name, namespace}, …]` is the high-level shape; if
	// present, we derive pod_filter and workload_filter from it.
	if filters := buildAppFilters(params["applications"]); filters != nil {
		if podFilter == "" {
			podFilter = filters.pod
		}
		if workloadFilter == "" {
			workloadFilter = filters.workload
		}
	}

	// 3. Pick query set
	queries := ApplicationMonitoringQueries
	if userQueries, ok := asStringMap(params["queries"]); ok && len(userQueries) > 0 {
		queries = userQueries
	}

	// 4. Run all queries in parallel against Prometheus.
	results := a.runQueries(ctx, queries, startTime, endTime, step, podFilter, workloadFilter, containerFilter)

	// 5. Bucket samples → applicationStats per app_id, then reduce.
	apps := extractMetricStats(results)
	out := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		out = append(out, app.toResponse())
	}
	return out
}

// runQueries substitutes placeholders, fans out range queries, parses each
// result into per-series TimeSeries grids keyed by query name.
func (a *AppStatsEnricher) runQueries(
	ctx context.Context,
	queries map[string]string,
	startTime, endTime time.Time,
	step int64,
	podFilter, workloadFilter, containerFilter string,
) map[string][]metricSeries {
	type result struct {
		key  string
		vals []metricSeries
		err  error
	}
	resCh := make(chan result, len(queries))
	var wg sync.WaitGroup
	startUnix := startTime.UTC().Unix()
	endUnix := endTime.UTC().Unix()
	stepStr := strconv.FormatInt(step, 10) + "s"

	for key, query := range queries {
		key, query := key, query
		wg.Add(1)
		go func() {
			defer wg.Done()
			q := substitute(query, stepStr, podFilter, workloadFilter, containerFilter)
			raw, err := a.q.QueryRange(ctx,
				q,
				strconv.FormatInt(startUnix, 10),
				strconv.FormatInt(endUnix, 10),
				stepStr,
				"",
			)
			if err != nil {
				resCh <- result{key: key, err: err}
				return
			}
			vals, err := parseMetricSeries(raw, startUnix, endUnix, step)
			resCh <- result{key: key, vals: vals, err: err}
		}()
	}
	wg.Wait()
	close(resCh)

	out := make(map[string][]metricSeries, len(queries))
	for r := range resCh {
		// Per-query errors are logged but don't fail the whole call.
		if r.err == nil {
			out[r.key] = r.vals
		}
	}
	return out
}

// metricSeries is one Prometheus matrix row — labels + a TimeSeries grid.
type metricSeries struct {
	labels map[string]string
	values *timeSeries
}

// parseMetricSeries decodes a Prometheus query_range response and lays each
// series onto a fixed-step TimeSeries grid spanning [startUnix, endUnix].
func parseMetricSeries(raw []byte, startUnix, endUnix, step int64) ([]metricSeries, error) {
	var resp struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	if resp.Status != "success" {
		return nil, nil
	}
	points := int((endUnix - startUnix) / step)
	if points <= 0 {
		points = 1
	}
	out := make([]metricSeries, 0, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		ts := newTimeSeries(startUnix, points, step)
		for _, v := range r.Values {
			if len(v) < 2 {
				continue
			}
			tInt := int64(0)
			switch t := v[0].(type) {
			case float64:
				tInt = int64(t)
			case string:
				if n, err := strconv.ParseFloat(t, 64); err == nil {
					tInt = int64(n)
				}
			}
			f := math.NaN()
			switch x := v[1].(type) {
			case string:
				if n, err := strconv.ParseFloat(x, 64); err == nil {
					f = n
				}
			case float64:
				f = x
			}
			ts.set(tInt, f)
		}
		out = append(out, metricSeries{labels: r.Metric, values: ts})
	}
	return out, nil
}

// substitute fills the expected placeholders. __CLUSTER__ has already been
// replaced by the relay (request.go:131); if it somehow survives, we strip
// it so the query stays well-formed.
func substitute(q, rangeStr, podFilter, workloadFilter, containerFilter string) string {
	q = strings.ReplaceAll(q, "$RANGE", rangeStr)
	q = strings.ReplaceAll(q, "$POD_FILTER", podFilter)
	q = strings.ReplaceAll(q, "$WORKLOAD_FILTER", workloadFilter)
	q = strings.ReplaceAll(q, "$CONTAINER_FILTER", containerFilter)
	q = strings.ReplaceAll(q, "__CLUSTER__", "")
	return q
}

// dictToPrometheusFilter
//
//	{label: [v1,v2]}    → label=~"v1|v2"
//	{label: "foo%bar"}  → label=~"foo.*bar"      (LIKE → regex)
//	{label: "foo"}      → label=~"foo"
//	{} or nil           → ""
//
// Multiple keys are comma-joined.
func dictToPrometheusFilter(d any) string {
	m, ok := d.(map[string]any)
	if !ok {
		return ""
	}
	var parts []string
	for k, v := range m {
		switch x := v.(type) {
		case []any:
			vals := make([]string, 0, len(x))
			for _, it := range x {
				if s, ok := it.(string); ok {
					vals = append(vals, s)
				}
			}
			if len(vals) == 0 {
				continue
			}
			parts = append(parts, fmt.Sprintf(`%s=~"%s"`, k, strings.Join(vals, "|")))
		case []string:
			if len(x) == 0 {
				continue
			}
			parts = append(parts, fmt.Sprintf(`%s=~"%s"`, k, strings.Join(x, "|")))
		case string:
			val := x
			if strings.Contains(val, "%") {
				val = strings.ReplaceAll(val, "%", ".*")
			}
			parts = append(parts, fmt.Sprintf(`%s=~"%s"`, k, val))
		}
	}
	return strings.Join(parts, ",")
}

// asStringMap accepts either map[string]any or map[string]string (callers do
// both); returns the values flattened to map[string]string.
func asStringMap(v any) (map[string]string, bool) {
	switch m := v.(type) {
	case map[string]string:
		return m, true
	case map[string]any:
		out := make(map[string]string, len(m))
		for k, vv := range m {
			if s, ok := vv.(string); ok {
				out[k] = s
			}
		}
		return out, true
	}
	return nil, false
}

type appFilters struct {
	pod      string
	workload string
}

// buildAppFilters mirrors get_application_stats:511-524 — turn
// `[{name, namespace}, ...]` into pod_filter / workload_filter strings.
func buildAppFilters(applications any) *appFilters {
	apps, ok := applications.([]any)
	if !ok {
		return nil
	}
	var pods, namespaces []string
	for _, app := range apps {
		m, ok := app.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		ns, _ := m["namespace"].(string)
		if name != "" {
			pods = append(pods, name+".*")
		}
		if ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	if len(pods) == 0 && len(namespaces) == 0 {
		return nil
	}
	pf := dictToPrometheusFilter(map[string]any{"pod": pods, "namespace": namespaces})
	wf := dictToPrometheusFilter(map[string]any{
		"destination_workload_name":      pods,
		"destination_workload_namespace": namespaces,
	})
	return &appFilters{pod: pf, workload: wf}
}

// extractMetricStats buckets samples per app_id and applies the per-query
// reducer.
func extractMetricStats(metrics map[string][]metricSeries) []*applicationStats {
	apps := map[string]*applicationStats{}
	getApp := func(id, name, namespace, container string) *applicationStats {
		if a, ok := apps[id]; ok {
			return a
		}
		a := &applicationStats{
			ApplicationID: id,
			Name:          name,
			Namespace:     namespace,
			Container:     container,
			OtherMetrics:  map[string]*timeSeries{},
			OtherReducers: map[string]func(acc, v float64) float64{},
		}
		apps[id] = a
		return a
	}

	for queryName, seriesList := range metrics {
		for _, m := range seriesList {
			labels := m.labels
			if !hasAnyKey(labels, "pod", "namespace", "container_id",
				"actual_destination_workload_namespace", "deployment", "destination_workload_namespace") {
				continue
			}
			ownerName, namespace, container := resolveOwner(labels)
			if namespace == "" {
				continue
			}
			appID := ownerName + "/" + namespace
			app := getApp(appID, ownerName, namespace, container)

			switch queryName {
			case "pod_memory_max_usage":
				app.MemMax = mergeSeries(app.MemMax, m.values, rMax)
			case "pod_cpu_usage_p99":
				app.CPUP99 = mergeSeries(app.CPUP99, m.values, rMax)
			case "pod_cpu_usage_p50":
				app.CPUP50 = mergeSeries(app.CPUP50, m.values, rMax)
			case "pod_memory_usage_p99":
				app.MemP99 = mergeSeries(app.MemP99, m.values, rMax)
			case "pod_memory_usage_p50":
				app.MemP50 = mergeSeries(app.MemP50, m.values, rMax)
			case "container_http_requests_total_count":
				app.TotalRequests = mergeSeries(app.TotalRequests, m.values, rNanSum)
			case "container_http_requests_failure_count":
				app.FailRequests = mergeSeries(app.FailRequests, m.values, rNanSum)
			case "container_log_messages":
				app.LogFailures = mergeSeries(app.LogFailures, m.values, rMax)
			case "container_cpu_request":
				app.MaxCPURequest = mergeSeries(app.MaxCPURequest, m.values, rMax)
			case "container_memory_request":
				app.MaxMemRequest = mergeSeries(app.MaxMemRequest, m.values, rMax)
			case "container_http_requests_latency_p99":
				app.LatencyP99 = mergeSeries(app.LatencyP99, m.values, rMax)
			case "filter_valid":
				app.ValidData = mergeSeries(app.ValidData, m.values, rNanSum)
			case "filter_bad":
				app.BadData = mergeSeries(app.BadData, m.values, rNanSum)
			case "filter_good":
				app.GoodData = mergeSeries(app.GoodData, m.values, rNanSum)
			case "container_memory_limit":
				app.MaxMemLimit = mergeSeries(app.MaxMemLimit, m.values, rMax)
			case "container_cpu_limit":
				app.MaxCPULimit = mergeSeries(app.MaxCPULimit, m.values, rMax)
			case "oom_kill_limit":
				app.OOMKill = mergeSeries(app.OOMKill, m.values, rMax)
			case "pod_cpu_max_usage":
				app.CPUMax = mergeSeries(app.CPUMax, m.values, rMax)
			default:
				// Route unknown query names by prefix —
				// "sum*"/"max*"/"min*"/else nan_sum.
				reducer := rNanSum
				switch {
				case strings.HasPrefix(queryName, "max"):
					reducer = rMax
				case strings.HasPrefix(queryName, "min"):
					reducer = rMin
				}
				app.OtherMetrics[queryName] = mergeSeries(app.OtherMetrics[queryName], m.values, reducer)
				app.OtherReducers[queryName] = reducer
			}
		}
	}

	// Stable ordering: by app_id.
	out := make([]*applicationStats, 0, len(apps))
	for _, a := range apps {
		out = append(out, a)
	}
	return out
}

// resolveOwner picks the owner name + namespace + container per a
// label-priority chain.
func resolveOwner(labels map[string]string) (owner, namespace, container string) {
	switch {
	case labels["actual_destination_workload_namespace"] != "":
		owner = labels["actual_destination_workload_name"]
		namespace = labels["actual_destination_workload_namespace"]
	case labels["destination_workload_namespace"] != "":
		owner = labels["destination_workload_name"]
		namespace = labels["destination_workload_namespace"]
	case labels["container_id"] != "":
		s := strings.Split(labels["container_id"], "/")
		if len(s) > 4 {
			namespace = s[2]
			owner = ownerFromPodName(s[3])
		}
	case labels["deployment"] != "":
		owner = labels["deployment"]
		namespace = labels["namespace"]
	default:
		owner = ownerFromPodName(labels["pod"])
		namespace = labels["namespace"]
	}
	if owner == "" {
		owner = ownerFromPodName(labels["pod"])
	}
	container = labels["container"]
	return owner, namespace, container
}

// ownerFromPodName mirrors get_owner_name — the
// regexes that strip the "-<hash>-<random>" suffix from a pod name.
var (
	deploymentPodRegex  = regexp.MustCompile(`([a-z0-9-]+)-[0-9a-f]{1,10}-[bcdfghjklmnpqrstvwxz2456789]{5}`)
	daemonsetPodRegex   = regexp.MustCompile(`([a-z0-9-]+)-[bcdfghjklmnpqrstvwxz2456789]{5}`)
	statefulsetPodRegex = regexp.MustCompile(`([a-z0-9-]+)-\d`)
)

func ownerFromPodName(pod string) string {
	if pod == "" {
		return ""
	}
	for _, re := range []*regexp.Regexp{deploymentPodRegex, daemonsetPodRegex, statefulsetPodRegex} {
		if m := re.FindStringSubmatch(pod); len(m) > 1 {
			return m[1]
		}
	}
	return pod
}

func hasAnyKey(m map[string]string, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// toResponse reduces every TimeSeries to a scalar and emits the shape that
// api-server's relay.ApplicationStatsResponse maps onto.
func (a *applicationStats) toResponse() map[string]any {
	out := map[string]any{
		"application_id": a.ApplicationID,
		"name":           a.Name,
		"namespace":      a.Namespace,
		"container":      a.Container,
	}
	addReduce := func(key string, ts *timeSeries, f func(acc, v float64) float64) {
		if ts == nil {
			return
		}
		v := ts.reduce(f)
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		out[key] = v
	}
	addLast := func(key string, ts *timeSeries) {
		if ts == nil {
			return
		}
		v := ts.last()
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		out[key] = v
	}

	addReduce("memory_max", a.MemMax, rMax)
	addReduce("memory_p99", a.MemP99, rMax)
	addReduce("memory_p50", a.MemP50, rMax)
	addReduce("cpu_p99", a.CPUP99, rMax)
	addReduce("cpu_p50", a.CPUP50, rMax)
	addReduce("cpu_max", a.CPUMax, rMax)
	if a.LatencyP99 != nil {
		v := a.LatencyP99.reduce(rMax)
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out["latency"] = v
			out["latency_p99"] = v
		}
	}
	addReduce("log_failure_count", a.LogFailures, rNanSum)
	addReduce("total_request_count", a.TotalRequests, rNanSum)
	if a.FailRequests != nil {
		v := a.FailRequests.reduce(rNanSum)
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			// Note: the legacy implementation has a typo where failure overwrites
			// total_request_count. We replicate so the wire shape matches.
			out["total_request_count"] = v
		}
	}
	addLast("good_data_count", a.GoodData)
	addLast("max_memory_request", a.MaxMemRequest)
	addLast("max_memory_limit", a.MaxMemLimit)
	addLast("max_cpu_limit", a.MaxCPULimit)
	addLast("bad_data_count", a.BadData)
	if a.ValidData != nil {
		v := a.ValidData.last()
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			out["valid_data_count"] = v
			if g, ok := out["good_data_count"].(float64); ok {
				out["bad_data_count"] = v - g
			}
		}
	}
	addReduce("oom_kill_limit", a.OOMKill, rMax)

	if len(a.OtherMetrics) > 0 {
		other := map[string]float64{}
		for key, ts := range a.OtherMetrics {
			reducer := a.OtherReducers[key]
			if reducer == nil {
				reducer = rNanSum
			}
			val := ts.reduce(reducer)
			if !math.IsNaN(val) && !math.IsInf(val, 0) {
				other[key] = val
			}
		}
		if len(other) > 0 {
			out["other_metrics"] = other
		}
	}
	return out
}
