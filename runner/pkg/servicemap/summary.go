package servicemap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	chclient "github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
)

// The summary tables hold what the service map reads from the raw
// otel_metrics_* tables, pre-aggregated per minute by materialized views as
// the collector inserts. A 24h map then reads a few hundred thousand rows
// instead of ~150M raw ones.
//
// Each MV runs inside the collector's insert into its source table, so a
// failing MV would fail that insert — and drop every metric, not just the
// service map's. The chart's exporter endpoint sets
// materialized_views_ignore_errors=1 so that a broken MV loses only summary
// rows. Each MV fails on its own, so the coverage check reads every failed MV
// run from system.query_views_log and refuses a window any of them touched;
// the map then falls back to Prometheus.
//
// The tables and MVs are versioned. Changing the metrics, labels or filters
// below means a new suffix, created next to the old one, never an edit of
// the old one: an MV's query cannot be altered in place, and rows already
// summed under the old definition would silently mix with the new. The
// change that adds _vN also drops the _vN-1 MVs and tables: windows from
// before _vN existed fall back to Prometheus, so nothing reads _vN-1 again,
// and its MVs would otherwise keep running inside every metric insert.
const (
	summaryEdgesTable   = "nb_service_map_edges_v1"
	summaryLatencyTable = "nb_service_map_latency_v1"
	summaryAgentsTable  = "nb_service_map_agents_v2"
	summaryPodsTable    = "nb_service_map_pods_v1"
	summaryMVSuffix     = "_mv"
	// summaryTTL matches the collector's 168h TTL on the raw tables.
	summaryTTL = "INTERVAL 7 DAY"
)

// summaryRetry is how often the setup retries while the raw tables do not
// exist yet (the exporter creates them on its first batch); summaryRecheck
// is how often it runs again once it has succeeded (recreating anything that
// went missing) or after ClickHouse refused the DDL.
var (
	summaryRetry   = time.Minute
	summaryRecheck = 10 * time.Minute
)

// retiredSummaryTables are earlier versions whose MVs and tables the setup
// drops: _v1 keyed agents by pod IP, so a new pod that took a gone pod's IP
// read as one agent with a gap and failed every window spanning both.
var retiredSummaryTables = []string{"nb_service_map_agents_v1"}

func summaryTables() []string {
	return []string{summaryEdgesTable, summaryLatencyTable, summaryAgentsTable, summaryPodsTable}
}

// summaryViews names the MVs, one per summary table and in the same order.
func summaryViews() []string {
	views := summaryTables()
	for i := range views {
		views[i] += summaryMVSuffix
	}
	return views
}

// errSummaryNotReady means the summary tables have not been created yet; the
// map is served from Prometheus meanwhile.
var errSummaryNotReady = errors.New("servicemap: clickhouse summary tables not created yet")

// edgeLabelColumns declares the edge labels as summary-table columns.
func edgeLabelColumns() string {
	cols := make([]string, len(edgeLabels))
	for i, l := range edgeLabels {
		cols[i] = l + " LowCardinality(String)"
	}
	return strings.Join(cols, ",\n\t\t")
}

// summaryDDL returns the statements that create the summary tables and then
// their MVs, all idempotent. Tables come first so an MV never points at a
// missing target.
func summaryDDL(db string) []string {
	by := strings.Join(edgeLabels, ", ")
	tables := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		minute DateTime,
		MetricName LowCardinality(String),
		%s,
		status LowCardinality(String),
		value Float64
	) ENGINE = SummingMergeTree
	PARTITION BY toDate(minute)
	ORDER BY (minute, MetricName, %s, status)
	TTL minute + %s`, table(db, summaryEdgesTable), edgeLabelColumns(), by, summaryTTL),

		// One row per raw series (label set × node agent) and minute, so the
		// read can take each series' mean and then the slowest per edge, as
		// PromQL does; a per-edge average would not match it.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		minute DateTime,
		MetricName LowCardinality(String),
		%s,
		series UInt64,
		total Float64,
		samples UInt64
	) ENGINE = SummingMergeTree
	PARTITION BY toDate(minute)
	ORDER BY (minute, MetricName, %s, series)
	TTL minute + %s`, table(db, summaryLatencyTable), edgeLabelColumns(), by, summaryTTL),

		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		minute DateTime,
		agent String,
		rows UInt64
	) ENGINE = SummingMergeTree
	PARTITION BY toDate(minute)
	ORDER BY (minute, agent)
	TTL minute + %s`, table(db, summaryAgentsTable), summaryTTL),

		// container → workload, the lookup that puts container health on its
		// app. Hourly: it changes only when pods do, and an extra container
		// from the hour before the window is never looked up.
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		hour DateTime,
		container_id String,
		src_workload_kind LowCardinality(String),
		src_workload_name LowCardinality(String),
		src_workload_namespace LowCardinality(String),
		rows UInt64
	) ENGINE = SummingMergeTree
	PARTITION BY toDate(hour)
	ORDER BY (hour, container_id, src_workload_kind, src_workload_name, src_workload_namespace)
	TTL hour + %s`, table(db, summaryPodsTable), summaryTTL),
	}

	views := summaryViews()
	mvs := []string{
		fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
	SELECT toStartOfMinute(TimeUnix) AS minute, MetricName, %s, Attributes['status'] AS status, sum(Value) AS value
	FROM %s
	WHERE MetricName IN (%s) AND %s
	GROUP BY minute, MetricName, %s, status`,
			table(db, views[0]), table(db, summaryEdgesTable),
			attrColumns(), table(db, rawSumTable), sqlList(counterMetrics()), deltaCounters, by),

		fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
	SELECT toStartOfMinute(TimeUnix) AS minute, MetricName, %s,
	       cityHash64(Attributes, ResourceAttributes['service.instance.id']) AS series,
	       sum(Sum) AS total, sum(Count) AS samples
	FROM %s
	WHERE MetricName IN (%s) AND %s
	GROUP BY minute, MetricName, %s, series`,
			table(db, views[1]), table(db, summaryLatencyTable),
			attrColumns(), table(db, rawHistogramTable), sqlList(latencyMetrics()), deltaHistograms, by),

		fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
	SELECT toStartOfMinute(TimeUnix) AS minute,
	       concat(ResourceAttributes['service.instance.id'], '/', ResourceAttributes['k8s.pod.uid']) AS agent, count() AS rows
	FROM %s
	WHERE MetricName = %s AND %s
	GROUP BY minute, agent`,
			table(db, views[2]), table(db, summaryAgentsTable),
			table(db, rawSumTable), sqlString(coverageMetric), deltaCounters),

		fmt.Sprintf(`CREATE MATERIALIZED VIEW IF NOT EXISTS %s TO %s AS
	SELECT toStartOfHour(TimeUnix) AS hour, Attributes['container_id'] AS container_id,
	       Attributes['src_workload_kind'] AS src_workload_kind, Attributes['src_workload_name'] AS src_workload_name,
	       Attributes['src_workload_namespace'] AS src_workload_namespace, count() AS rows
	FROM %s
	WHERE MetricName = %s AND %s
	GROUP BY hour, container_id, src_workload_kind, src_workload_name, src_workload_namespace`,
			table(db, views[3]), table(db, summaryPodsTable),
			table(db, rawSumTable), sqlString(coverageMetric), deltaCounters),
	}
	return append(tables, mvs...)
}

// retiredSummaryDDL drops the retired versions, each MV before its table so
// no insert writes into a missing table.
func retiredSummaryDDL(db string) []string {
	var stmts []string
	for _, t := range retiredSummaryTables {
		stmts = append(stmts,
			fmt.Sprintf("DROP VIEW IF EXISTS %s", table(db, t+summaryMVSuffix)),
			fmt.Sprintf("DROP TABLE IF EXISTS %s", table(db, t)))
	}
	return stmts
}

// rawTablesExist reports whether the collector has created both source tables
// the MVs read from.
func rawTablesExist(ctx context.Context, c *chclient.Client) (bool, error) {
	rows, err := chRows(ctx, c, fmt.Sprintf(`SELECT count() FROM system.tables WHERE database = %s AND name IN (%s)`,
		sqlString(c.Database), sqlList([]string{rawSumTable, rawHistogramTable})))
	if err != nil {
		return false, err
	}
	return len(rows) == 1 && len(rows[0]) == 1 && num(rows[0][0]) == 2, nil
}

// ensureSummary creates the summary tables and MVs once the raw tables exist.
// It returns errSummaryNotReady while they don't.
func ensureSummary(ctx context.Context, c *chclient.Client) error {
	ok, err := rawTablesExist(ctx, c)
	if err != nil {
		return err
	}
	if !ok {
		return errSummaryNotReady
	}
	for _, stmt := range summaryDDL(c.Database) {
		if _, err := chRows(ctx, c, stmt); err != nil {
			return fmt.Errorf("servicemap: create summary: %w", err)
		}
	}
	return nil
}

// dropRetiredSummary drops the retired versions. It is cleanup only: the new
// version does not need it, so a failure (say, a user that may CREATE but not
// DROP) must not keep the summary from being ready.
func dropRetiredSummary(ctx context.Context, c *chclient.Client) error {
	for _, stmt := range retiredSummaryDDL(c.Database) {
		if _, err := chRows(ctx, c, stmt); err != nil {
			return fmt.Errorf("servicemap: drop retired summary: %w", err)
		}
	}
	return nil
}

// StartSummary creates the summary tables in the background and marks them
// ready for Build, then keeps re-running the (idempotent) setup until ctx
// ends, so tables or MVs that disappear — say ClickHouse restarted on an
// empty volume — are created again. Reads do not trust the ready flag alone:
// the coverage check confirms every MV exists and dates the summary from the
// newest one. It does nothing for the prometheus source, so a default install
// creates nothing in ClickHouse.
//
// The retired versions are dropped once, after the first successful setup,
// not on every recheck: during a rolling update or a rollback an older runner
// re-creates them, and dropping on every pass would make the two fight.
func (s *Service) StartSummary(ctx context.Context) {
	if s.CH == nil || s.Source == "" || s.Source == SourcePrometheus {
		return
	}
	logger := s.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	go func() {
		failing, dropped := false, false
		for {
			err := ensureSummary(ctx, s.CH)
			wait := summaryRecheck
			switch {
			case err == nil:
				failing = false
				if !s.summaryReady.Swap(true) {
					logger.Info("service map summary tables ready", "edges", summaryEdgesTable)
				}
				if !dropped {
					dropped = true
					if err := dropRetiredSummary(ctx, s.CH); err != nil {
						logger.Warn("cannot drop retired service map summary tables; leaving them", "err", err)
					}
				}
			case errors.Is(err, errSummaryNotReady):
				s.summaryReady.Store(false)
				wait = summaryRetry
			case !failing:
				logger.Warn("cannot create service map summary tables; serving the map from prometheus", "err", err)
				failing = true
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}()
}
