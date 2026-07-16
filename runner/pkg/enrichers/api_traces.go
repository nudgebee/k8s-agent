package enrichers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nudgebee/nudgebee-agent/pkg/clickhouse"
	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
)

// APITracesEnricher implements `api_traces_enricher_v2` — fetches OTel
// traces for a workload (or by trace_id) and returns the rows in a
// Finding-shaped JsonBlock.
//
// The api-server caller
// passes destination_workload_{name,namespace} + duration_minutes and
// reads the result via relay.FormatEvidenceResponseFromAgent — i.e. the
// Finding shape with a JsonBlock whose `data` is
// `{"name":"api_traces_enricher","data":[<row maps>]}`.
//
// When ClickHouse isn't configured we still return success with an empty
// data array so the caller's response stays parseable.
type APITracesEnricher struct {
	ch        *clickhouse.Client
	accountID string
}

// NewAPITracesEnricher wires the action to a ClickHouse client (may be nil).
func NewAPITracesEnricher(ch *clickhouse.Client, accountID string) *APITracesEnricher {
	return &APITracesEnricher{ch: ch, accountID: accountID}
}

// Handler returns the dispatch.Handler.
func (a *APITracesEnricher) Handler() dispatch.Handler {
	return func(ctx context.Context, params map[string]any) (any, error) {
		body := buildTracesPayload(ctx, a.ch, params)
		jb, err := JSONBlock(body)
		if err != nil {
			return nil, err
		}
		return FindingResponse(a.accountID, uuid.Nil, jb)
	}
}

// buildTracesPayload runs the SQL and assembles the
// {name, data, [error]} dict the api-server expects.
func buildTracesPayload(ctx context.Context, ch *clickhouse.Client, params map[string]any) map[string]any {
	out := map[string]any{
		"name": "api_traces_enricher",
		"data": []map[string]any{},
	}
	if ch == nil {
		// ClickHouse isn't configured on this agent. Return an empty—but
		// successful—traces payload (no `error` key) so the backend traces
		// parser renders "no traces" instead of surfacing a hard error to
		// the user. Matches the legacy nudgebee_actions.get_application_traces,
		// which returns an empty result when the trace store is absent.
		return out
	}

	dstName, _ := params["destination_workload_name"].(string)
	dstNs, _ := params["destination_workload_namespace"].(string)
	traceID, _ := params["trace_id"].(string)
	if (dstName == "" || dstNs == "") && traceID == "" {
		// Mirrors api_traces_enricher_v2's early return (line 670-675).
		out["error"] = "destination_workload_name and destination_workload_namespace not provided"
		return out
	}

	durationMins := 15
	if n, err := toInt(params["duration_minutes"]); err == nil && n > 0 {
		durationMins = n
	}
	maxTraces := 100
	if n, err := toInt(params["max_traces"]); err == nil && n > 0 {
		maxTraces = n
	}
	orderBy, _ := params["order_by"].(string)
	if orderBy == "" {
		orderBy = "Timestamp desc"
	}
	// "Safe" order_by — refuse anything that has SQL keywords we don't
	// expect, since this gets concat'd. (The legacy implementation has
	// the same implicit trust; we tighten slightly.)
	if !isSafeOrderBy(orderBy) {
		orderBy = "Timestamp desc"
	}

	statusCodes := stringSliceFromParam(params["status_code"])

	endTime := time.Now().UTC()
	startTime := endTime.Add(-time.Duration(durationMins) * time.Minute)
	rStart := startTime.Format("2006-01-02T15:04:05.000000Z")
	rEnd := endTime.Format("2006-01-02T15:04:05.000000Z")

	var query string
	if traceID != "" {
		query = "SELECT * FROM otel_traces WHERE TraceId = '" + escapeSQL(extractTraceID(traceID)) +
			"' ORDER BY " + orderBy + " LIMIT " + strconv.Itoa(maxTraces)
	} else {
		// Default to the non-materialized path used when trace_provider_config
		// is missing or hasMaterializedColumn=false. It's the safer general case.
		dstNameExpr := connectedWorkloadExpr("destination")
		dstNsExpr := connectedNamespaceExpr("destination")
		srcNameExpr := connectedWorkloadExpr("source")
		srcNsExpr := connectedNamespaceExpr("source")
		nameSet := "('" + escapeSQL(dstName) + "')"
		nsSet := "('" + escapeSQL(dstNs) + "')"
		statusFilter := ""
		if len(statusCodes) > 0 {
			parts := make([]string, 0, len(statusCodes))
			for _, c := range statusCodes {
				parts = append(parts, "'"+escapeSQL(c)+"'")
			}
			statusFilter = " AND StatusCode in (" + strings.Join(parts, ",") + ")"
		}
		query = fmt.Sprintf(`SELECT * FROM otel_traces
WHERE ((Timestamp >= parseDateTimeBestEffort('%s')) AND (Timestamp <= parseDateTimeBestEffort('%s')))
AND ((%s in %s AND %s in %s)
     OR (%s = '%s' AND %s = '%s'))
%s
ORDER BY %s LIMIT %d`,
			escapeSQL(rStart), escapeSQL(rEnd),
			dstNameExpr, nameSet, dstNsExpr, nsSet,
			srcNameExpr, escapeSQL(dstName), srcNsExpr, escapeSQL(dstNs),
			statusFilter, orderBy, maxTraces)
	}

	res, err := ch.Query(ctx, query, nil)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	if res.Error != nil {
		out["error"] = *res.Error
		return out
	}
	// Convert column-major (data, columns) → row-list of {col: val} matching
	// the legacy `[dict(zip(columns, row)) for row in result]`.
	rows := make([]map[string]any, 0, len(res.Data))
	for _, row := range res.Data {
		m := make(map[string]any, len(res.Columns))
		for i, col := range res.Columns {
			if i < len(row) {
				m[col] = row[i]
			}
		}
		rows = append(rows, m)
	}
	out["data"] = rows
	return out
}

// connectedWorkloadExpr / connectedNamespaceExpr build a CASE/WHEN
// fallback chain — try SpanAttributes, then ResourceAttributes, then a
// Coroot/OTLP-specific fallback.
func connectedWorkloadExpr(role string) string {
	return `(CASE
        WHEN mapContains(SpanAttributes, '` + role + `.workload_name') THEN SpanAttributes['` + role + `.workload_name']
        WHEN mapContains(ResourceAttributes, 'k8s.deployment.name') THEN ResourceAttributes['k8s.deployment.name']
        WHEN mapContains(ResourceAttributes, 'service.name') THEN ResourceAttributes['service.name']
        ELSE ResourceAttributes['net.peer.name']
    END)`
}

func connectedNamespaceExpr(role string) string {
	return `(CASE
        WHEN mapContains(SpanAttributes, '` + role + `.workload_namespace') THEN SpanAttributes['` + role + `.workload_namespace']
        WHEN mapContains(ResourceAttributes, 'k8s.namespace.name') THEN ResourceAttributes['k8s.namespace.name']
        ELSE ResourceAttributes['service.namespace']
    END)`
}

// extractTraceID accepts either a raw 32-hex trace id or a W3C traceparent
// (`02-<32hex>-<16hex>-01`) and returns the trace_id portion. Mirrors
// is_valid_traceparent + extract_trace_id at the backend.
func extractTraceID(s string) string {
	if strings.Count(s, "-") == 3 {
		parts := strings.Split(s, "-")
		if len(parts[1]) == 32 {
			return parts[1]
		}
	}
	return s
}

// stringSliceFromParam accepts either []string, []any of strings, or a
// single string and returns a []string. Mirrors the loose typing api-server
// callers use for status_code.
func stringSliceFromParam(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case string:
		if x == "" {
			return nil
		}
		return []string{x}
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// isSafeOrderBy is a small allowlist: the column name must look like a SQL
// identifier (alpha + dot only) and the optional direction is asc/desc.
// The legacy implementation concatenates order_by directly into the
// query — we tighten so callers can't smuggle arbitrary SQL.
func isSafeOrderBy(s string) bool {
	for _, part := range strings.Split(s, ",") {
		fields := strings.Fields(strings.TrimSpace(part))
		if len(fields) == 0 || len(fields) > 2 {
			return false
		}
		col := fields[0]
		for _, r := range col {
			if r != '_' && r != '.' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
				return false
			}
		}
		if len(fields) == 2 {
			d := strings.ToLower(fields[1])
			if d != "asc" && d != "desc" {
				return false
			}
		}
	}
	return true
}

// escapeSQL is a single-quote escape for ClickHouse — the binary protocol
// has parameterized queries, but the HTTP path we use doesn't accept binds,
// so we string-escape. ClickHouse uses `”` to quote a single quote.
func escapeSQL(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
