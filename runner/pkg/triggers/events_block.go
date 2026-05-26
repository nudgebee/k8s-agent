package triggers

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// recentEventsTimeout caps the K8s API call EnrichBlocks makes when
// fetching events. The matcher path is hot — never block the whole
// trigger pipeline on a slow API server. If the fetch times out we
// emit a placeholder block so users still see "we tried" in the UI
// rather than no evidence at all.
const recentEventsTimeout = 3 * time.Second

// recentEventsLimit caps rows for the subject-scoped events table.
// pod_events_enricher truncates at 30 by default; same here.
const recentEventsLimit = 30

// supplementaryEventsLimit caps rows for the node + namespace tables
// the engine appends as extra context. Smaller than the subject limit
// so the Finding card stays readable.
const supplementaryEventsLimit = 10

// recentEventsTable returns the `type: "table"` evidence block listing
// the most recent K8s events scoped by the (namespace, kind, name)
// triple. Each dimension can be empty for broader scopes:
//
//   - subject:   namespace=ns, kind=Pod, name=web-0
//   - node:      namespace="", kind="Node", name=<node>
//   - namespace: namespace=ns, kind="", name=""
//
// Returns nil when no lister is wired, when the lister errored, or
// when there are no events to surface.
func recentEventsTable(ec EnrichContext, namespace, kind, name, title string, limit int) []EvidenceBlock {
	if ec.EventsLister == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), recentEventsTimeout)
	defer cancel()
	events, err := ec.EventsLister.ListEvents(ctx, namespace, kind, name, limit)
	if err != nil || len(events) == 0 {
		return nil
	}
	// Most-recent first.
	sort.Slice(events, func(i, j int) bool {
		return events[i].LastSeen.After(events[j].LastSeen)
	})
	headers := []any{"Last Seen", "Type", "Reason", "Source", "Count", "Message"}
	rows := make([]any, 0, len(events))
	for _, e := range events {
		count := int(e.Count)
		if count == 0 {
			count = 1
		}
		rows = append(rows, []any{
			formatEventTimeAgo(e.LastSeen),
			e.Type,
			e.Reason,
			e.Source,
			fmt.Sprintf("%d", count),
			truncate(e.Message, 200),
		})
	}
	return []EvidenceBlock{{
		"type": "table",
		"data": map[string]any{
			"table_name":       title,
			"headers":          headers,
			"rows":             rows,
			"column_renderers": map[string]any{},
		},
		"additional_info": map[string]any{
			"object_kind":      kind,
			"object_namespace": namespace,
			"object_name":      name,
			"event_count":      len(events),
		},
	}}
}

// formatEventTimeAgo renders an event timestamp as "1m ago" / "12s ago"
// / "3h ago" — same shape `kubectl get events` and the UI use.
// Falls back to RFC3339 when the timestamp is zero (e.g. truncated
// event payload from older K8s versions).
func formatEventTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return t.UTC().Format(time.RFC3339)
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
