package enrichers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This file is a measurement harness, not an assertion suite. It reproduces the
// wire-bytes -> live-heap amplification observed on the dev runner, where
// ~76-150MB of Prometheus JSON turned into 1.7-4.9GiB of live Go heap during
// prometheus_queries_enricher bursts.
//
// Run it with:
//
//	go test ./pkg/enrichers/ -run 'TestAmplification' -v
//	PROM_BENCH_QUERIES=10 PROM_BENCH_SERIES=200 go test ./pkg/enrichers/ -run 'TestAmplification' -v -timeout 20m
//
// Peak heap is sampled from runtime.MemStats.HeapInuse, the same quantity
// Prometheus exports as go_memstats_heap_inuse_bytes, so the numbers here are
// directly comparable to the production series.

const (
	defaultBenchQueries = 4
	defaultBenchSeries  = 100
	defaultBenchSamples = 1440 // 24h at a 60s step
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func benchDims() (queries, series, samples int) {
	return envInt("PROM_BENCH_QUERIES", defaultBenchQueries),
		envInt("PROM_BENCH_SERIES", defaultBenchSeries),
		envInt("PROM_BENCH_SAMPLES", defaultBenchSamples)
}

// buildMatrixBody renders a Prometheus query_range response with the label
// cardinality a real container-metric query carries. Sample encoding matches
// what Prometheus/VictoriaMetrics emit: [<unix seconds>,"<value>"].
func buildMatrixBody(series, samples int) []byte {
	type sample = []any
	type oneSeries struct {
		Metric map[string]string `json:"metric"`
		Values []sample          `json:"values"`
	}
	out := make([]oneSeries, 0, series)
	base := int64(1_700_000_000)
	for s := range series {
		vals := make([]sample, 0, samples)
		for i := range samples {
			vals = append(vals, sample{
				base + int64(i)*60,
				strconv.FormatFloat(float64(s*1000+i)/7.0, 'f', 6, 64),
			})
		}
		out = append(out, oneSeries{
			Metric: map[string]string{
				"__name__":  "container_memory_working_set_bytes",
				"container": fmt.Sprintf("container-%d", s%7),
				"endpoint":  "https-metrics",
				"id":        fmt.Sprintf("/kubepods/burstable/pod%08d/%016x", s, s*2654435761),
				"image":     fmt.Sprintf("registry.example.com/team/service-%d:1.42.0-rc.3", s%23),
				"instance":  fmt.Sprintf("10.64.%d.%d:10250", s/250, s%250),
				"job":       "kubelet",
				"name":      fmt.Sprintf("k8s_container-%d_workload-%d-7d9f8b5c4d-abcde_namespace-%d", s%7, s, s%17),
				"namespace": fmt.Sprintf("namespace-%d", s%17),
				"node":      fmt.Sprintf("gke-cluster-node-pool-v3-%08x-%04x", s*2246822519, s),
				"pod":       fmt.Sprintf("workload-%d-7d9f8b5c4d-%05x", s, s),
				"service":   "kubelet",
			},
			Values: vals,
		})
	}
	body, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "matrix", "result": out},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// copyingProm hands every call its own copy of the canned body, mirroring
// production where each query allocates a fresh response via io.ReadAll.
type copyingProm struct {
	body  []byte
	bytes atomic.Int64
}

func (c *copyingProm) Query(context.Context, string, string, string) ([]byte, error) {
	return c.next(), nil
}

func (c *copyingProm) QueryRange(context.Context, string, string, string, string, string) ([]byte, error) {
	return c.next(), nil
}

func (c *copyingProm) LabelValues(context.Context, string, string, string, []string) ([]byte, error) {
	return []byte(`{"status":"success","data":[]}`), nil
}

func (c *copyingProm) next() []byte {
	b := make([]byte, len(c.body))
	copy(b, c.body)
	c.bytes.Add(int64(len(b)))
	return b
}

// heapSampler polls HeapInuse on a ticker and records the maximum. HeapInuse is
// the live-heap figure; it is what blows past the container limit and triggers
// the OOMKill, so it is the number worth optimising.
type heapSampler struct {
	stop chan struct{}
	done chan struct{}
	peak atomic.Uint64
}

func startHeapSampler(interval time.Duration) *heapSampler {
	h := &heapSampler{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		t := time.NewTicker(interval)
		defer t.Stop()
		var ms runtime.MemStats
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				runtime.ReadMemStats(&ms)
				for {
					cur := h.peak.Load()
					if ms.HeapInuse <= cur || h.peak.CompareAndSwap(cur, ms.HeapInuse) {
						break
					}
				}
			}
		}
	}()
	return h
}

func (h *heapSampler) finish() uint64 {
	close(h.stop)
	<-h.done
	return h.peak.Load()
}

func mib(n uint64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

// measure runs fn with a fresh heap baseline and reports peak live heap and
// total bytes allocated over the call.
func measure(fn func()) (peakHeap, totalAlloc uint64) {
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	s := startHeapSampler(2 * time.Millisecond)
	fn()
	peakHeap = s.finish()

	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	return peakHeap, after.TotalAlloc - before.TotalAlloc
}

// TestAmplification_PrometheusQueriesEnricher walks the full
// prometheus_queries_enricher path and reports, stage by stage, how many bytes
// of live heap each stage costs per byte on the wire.
func TestAmplification_PrometheusQueriesEnricher(t *testing.T) {
	// This reports numbers rather than asserting on them — there is no
	// threshold that would be stable across machines. Skip it in -short runs
	// so CI does not pay for a test that cannot fail.
	if testing.Short() {
		t.Skip("measurement harness; run without -short to get the numbers")
	}
	nQueries, nSeries, nSamples := benchDims()
	body := buildMatrixBody(nSeries, nSamples)
	wire := uint64(len(body)) * uint64(nQueries)

	t.Logf("config: queries=%d series=%d samples=%d", nQueries, nSeries, nSamples)
	t.Logf("wire:   %s per query, %s total (%d samples)",
		mib(uint64(len(body))), mib(wire), nQueries*nSeries*nSamples)

	rawQueries := make([]any, 0, nQueries)
	for i := range nQueries {
		rawQueries = append(rawQueries, map[string]any{
			"key":   fmt.Sprintf("Q%d", i),
			"query": fmt.Sprintf(`sum by (pod) (rate(container_cpu_usage_seconds_total{namespace="ns-%d"}[5m]))`, i),
		})
	}
	params := map[string]any{
		"promql_queries": rawQueries,
		"duration": map[string]any{
			"starts_at": "2026-09-14 08:00:00 UTC",
			"ends_at":   "2026-09-15 08:00:00 UTC",
		},
		"steps": "60",
	}

	type stage struct {
		name string
		run  func(p *PrometheusEnricher) any
	}

	// Each stage is cumulative: it includes everything the previous one did.
	stages := []stage{
		{"1. decode only (PrometheusQueryResultDict)", func(p *PrometheusEnricher) any {
			results := make([]map[string]any, nQueries)
			var wg sync.WaitGroup
			for i := range nQueries {
				wg.Add(1)
				go func() {
					defer wg.Done()
					raw, _ := p.q.QueryRange(context.Background(), "q", "0", "1", "60", "")
					d, err := PrometheusQueryResultDict(raw)
					if err != nil {
						t.Error(err)
					}
					results[i] = d
				}()
			}
			wg.Wait()
			return results
		}},
		{"2. + JSONBlock (marshal + string)", func(p *PrometheusEnricher) any {
			results := make([]map[string]any, nQueries)
			var wg sync.WaitGroup
			for i := range nQueries {
				wg.Add(1)
				go func() {
					defer wg.Done()
					raw, _ := p.q.QueryRange(context.Background(), "q", "0", "1", "60", "")
					results[i], _ = PrometheusQueryResultDict(raw)
				}()
			}
			wg.Wait()
			out := make(map[string]any, nQueries)
			for i, r := range results {
				out[fmt.Sprintf("Q%d", i)] = r
			}
			block, err := JSONBlock(out)
			if err != nil {
				t.Fatal(err)
			}
			return block
		}},
		{"3. full HandleQueriesEnricher (+ FindingResponse)", func(p *PrometheusEnricher) any {
			resp, err := p.HandleQueriesEnricher(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			return resp
		}},
		{"4. + relay marshal (what actually ships)", func(p *PrometheusEnricher) any {
			resp, err := p.HandleQueriesEnricher(context.Background(), params)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(resp)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}},
	}

	// Peak heap depends on where the GC happens to run, so take the median of
	// several passes rather than a single sample.
	const passes = 5
	t.Logf("")
	t.Logf("%-52s %14s %14s %8s", "stage", "peak heap", "total alloc", "x wire")
	for _, st := range stages {
		peaks := make([]uint64, 0, passes)
		totals := make([]uint64, 0, passes)
		for range passes {
			fake := &copyingProm{body: body}
			p := &PrometheusEnricher{q: fake, accountID: "acct"}
			var keep any
			peak, total := measure(func() { keep = st.run(p) })
			runtime.KeepAlive(keep)
			peaks = append(peaks, peak)
			totals = append(totals, total)
		}
		slices.Sort(peaks)
		slices.Sort(totals)
		medPeak, medTotal := peaks[passes/2], totals[passes/2]
		t.Logf("%-52s %14s %14s %7.1fx", st.name, mib(medPeak), mib(medTotal),
			float64(medPeak)/float64(wire))
	}

	// Final payload size, for reference against the wire bytes that produced it.
	fake := &copyingProm{body: body}
	p := &PrometheusEnricher{q: fake, accountID: "acct"}
	resp, err := p.HandleQueriesEnricher(context.Background(), params)
	if err != nil {
		t.Fatal(err)
	}
	shipped, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("")
	t.Logf("shipped payload: %s (%.2fx wire)", mib(uint64(len(shipped))), float64(len(shipped))/float64(wire))
}

// BenchmarkQueriesEnricher gives allocation volume per call; run with -benchmem.
func BenchmarkQueriesEnricher(b *testing.B) {
	nQueries, nSeries, nSamples := benchDims()
	body := buildMatrixBody(nSeries, nSamples)

	rawQueries := make([]any, 0, nQueries)
	for i := range nQueries {
		rawQueries = append(rawQueries, map[string]any{
			"key":   fmt.Sprintf("Q%d", i),
			"query": fmt.Sprintf("query_%d", i),
		})
	}
	params := map[string]any{
		"promql_queries": rawQueries,
		"duration": map[string]any{
			"starts_at": "2026-09-14 08:00:00 UTC",
			"ends_at":   "2026-09-15 08:00:00 UTC",
		},
		"steps": "60",
	}

	fake := &copyingProm{body: body}
	p := &PrometheusEnricher{q: fake, accountID: "acct"}

	b.SetBytes(int64(len(body)) * int64(nQueries))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		resp, err := p.HandleQueriesEnricher(context.Background(), params)
		if err != nil {
			b.Fatal(err)
		}
		out, err := json.Marshal(resp)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(out)
	}
}

// BenchmarkDecodeOnly isolates the Prometheus matrix decode — the stage that
// dominated the runner's memory bursts. Run with -benchmem.
func BenchmarkDecodeOnly(b *testing.B) {
	_, nSeries, nSamples := benchDims()
	body := buildMatrixBody(nSeries, nSamples)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := PrometheusQueryResultDict(body); err != nil {
			b.Fatal(err)
		}
	}
}
