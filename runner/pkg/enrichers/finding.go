// Package enrichers implements enricher actions over the agent's primitives.
//
// These wrap thin observability calls (Prometheus, Loki, kube logs, …) in the
// wire shape that today's api-server / llm-server callers expect:
//
//	{
//	  "success": true,
//	  "findings": [{
//	    "evidence": [{
//	      "data": "<json-encoded array of typed blocks>",
//	      "issue_id": "<uuid>",
//	      "file_type": "structured_data",
//	      "account_id": "..."
//	    }]
//	  }]
//	}
//
// Why this lives in the agent today: api-server's playbook actions still call
// these by name (prometheus_enricher / prometheus_queries_enricher / …) and
// expect the Finding back. Eventually api-server gets its own enrichers
// package and this layer goes away (plan §5c).
package enrichers

import (
	"encoding/base64"
	"encoding/json"

	"github.com/google/uuid"
)

// Block is the shape of one element inside `evidence[0].data` (a JSON-encoded
// array). Each block class becomes one of these. We keep the shape open
// with map[string]any rather than a typed struct because different block
// types have wildly different payloads (PrometheusBlock has data+metadata+
// version, JsonBlock has stringified data, FileBlock has filename+data, …).
type Block map[string]any

// PrometheusBlock builds a PrometheusBlock evidence element. `data`
// must already be in PrometheusQueryResult.dict() shape:
//
//	{ "result_type": "vector", "vector_result": [...], "series_list_result": null,
//	  "scalar_result": null, "string_result": null }
func PrometheusBlock(data map[string]any, query string) Block {
	return Block{
		"type": "prometheus",
		"data": data,
		"metadata": map[string]any{
			"query-result-version": "1.0",
			"query":                query,
		},
		"version":         1.0,
		"additional_info": nil,
	}
}

// JSONBlock builds a JsonBlock evidence element. The inner `data` is the
// JSON-encoded string (so a single string field, not nested JSON).
// prometheus_queries_enricher uses this with a
// {key: PrometheusQueryResult.dict()} payload.
func JSONBlock(payload any) (Block, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return Block{
		"type":            "json",
		"data":            string(b),
		"additional_info": nil,
	}, nil
}

// FileBlock builds a FileBlock evidence element.
//
// Wire shape includes the `str(base64.b64encode(bytes))` Python quirk that
// wraps the encoded payload with a literal `b'...'`. UI consumers
// (KubernetesPodYaml.tsx, KubernetesPodLogs.tsx, KubernetesWorkloads.jsx)
// all decode with `atob(d.data.slice(2, -1))` — strip 2 chars from the
// front (`b'`), 1 from the back (`'`), then base64-decode. Without the
// wrapping the slice eats real data.
//
// Caller responsibility: gzip first when the file is "text" (the legacy
// pipeline gzips `.txt`/`.log`). For logs, pass the gzipped bytes and a
// filename ending in `.log.gz` so `type` becomes `"gz"` (UI matches on
// `type === 'gz'`). For yaml, pass raw bytes and `<name>.yaml` so `type`
// becomes `"yaml"`.
func FileBlock(filename string, contents []byte) Block {
	ext := ""
	for i := len(filename) - 1; i >= 0; i-- {
		if filename[i] == '.' {
			ext = filename[i+1:]
			break
		}
	}
	encoded := base64.StdEncoding.EncodeToString(contents)
	return Block{
		"type":     ext,
		"filename": filename,
		// `b'...'` mirrors Python's `str(base64.b64encode(bytes))`. The UI
		// strips the wrapping unconditionally; emitting bare base64 here
		// would chop two chars of real data.
		"data":            "b'" + encoded + "'",
		"additional_info": nil,
	}
}

// MarkdownBlock builds a MarkdownBlock evidence element. `text` is the
// (already-Github-flavored) markdown body; we don't re-run a
// `to_github_markdown` transformer because none of our existing call
// sites need the conversion.
func MarkdownBlock(text string) Block {
	return Block{
		"type":            "markdown",
		"data":            text,
		"additional_info": nil,
	}
}

// FindingResponse builds the full Finding-shaped response that api-server's
// `relay.ExecuteAndExtractResponse` walks.
// Pass one or more Block values; they all land inside the same evidence element's
// `data` array (the structured_data list inside one Enrichment).
//
// `accountID` is the tenant id; `findingID` is a stable UUID used as `issue_id`.
// If findingID is the zero value a fresh UUID is generated.
func FindingResponse(accountID string, findingID uuid.UUID, blocks ...Block) (map[string]any, error) {
	if findingID == uuid.Nil {
		findingID = uuid.New()
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, err
	}
	evidence := map[string]any{
		"issue_id":   findingID.String(),
		"file_type":  "structured_data",
		"data":       string(encoded),
		"account_id": accountID,
	}
	finding := map[string]any{
		"id":       findingID.String(),
		"evidence": []any{evidence},
	}
	return map[string]any{
		"success":  true,
		"findings": []any{finding},
	}, nil
}

// ErrorResponse returns the legacy error-response shape. Returned by
// handlers when input is bad or the underlying datasource fails.
func ErrorResponse(msg string, code int) map[string]any {
	return map[string]any{
		"success":    false,
		"msg":        msg,
		"error_code": code,
	}
}
