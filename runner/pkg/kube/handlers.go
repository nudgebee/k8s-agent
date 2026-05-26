package kube

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"sigs.k8s.io/yaml"

	"github.com/nudgebee/nudgebee-agent/pkg/dispatch"
	"github.com/nudgebee/nudgebee-agent/pkg/enrichers"
)

// Handlers wires Group B primitives into the dispatch registry.
//
// Every response is wrapped in the Finding envelope
// `{success, findings:[{evidence:[{data: <stringified JsonBlock array>}]}]}`
// — each `@action` calls `event.add_enrichment([JsonBlock(...)])`.
// UI/api-server callers walk that envelope to find the inner data:
//
//	res.data.findings[0].evidence[0].data → JSON-string of [{type:"json", data:"<inner-json>", ...}]
//	→ JSON.parse[0].data → JSON.parse → actual payload
//
// Without this wrapper, callers like KubernetesPVC.jsx see undefined at the
// findings[] hop and render an empty table even though the relay round-tripped
// the K8s list correctly.
func Handlers(c *Client, k *KubectlExecutor, accountID string) map[string]dispatch.Handler {
	hs := map[string]dispatch.Handler{}
	if c != nil {
		hs["get_resource"] = func(ctx context.Context, p map[string]any) (any, error) {
			data, err := c.GetResource(ctx, ParseGetParams(p))
			if err != nil {
				return nil, err
			}
			// camelCase → snake_case for parity with the legacy Hikaru output —
			// see snake.go for the why and which fields are skipped.
			return wrapAsFinding(accountID, SnakeKeysDeep(data))
		}
		hs["get_resource_yaml"] = func(ctx context.Context, p map[string]any) (any, error) {
			params := ParseGetParams(p)
			data, err := c.GetResource(ctx, params)
			if err != nil {
				return nil, err
			}
			y, err := yaml.Marshal(SnakeKeysDeep(data))
			if err != nil {
				return nil, err
			}
			// get_resource_yaml emits a MarkdownBlock header + a
			// FileBlock(<name>.yaml, raw_bytes). `.yaml` is not a text-file
			// (only .txt/.log are), so the YAML is sent as-is,
			// base64-wrapped — KubernetesPodYaml.tsx:50-52 matches on
			// `type === "yaml"` and decodes with `atob(d.data.slice(2, -1))`.
			filename := params.Name
			if filename == "" {
				filename = params.ResourceType
			}
			filename += ".yaml"
			kind := params.ResourceType
			if params.Name != "" {
				kind = params.Name
			}
			markdown := enrichers.MarkdownBlock(fmt.Sprintf("Your YAML file for %s %s/%s",
				params.ResourceType, params.Namespace, kind))
			file := enrichers.FileBlock(filename, y)
			return enrichers.FindingResponse(accountID, uuid.Nil, markdown, file)
		}
		hs["list_resource_names"] = func(ctx context.Context, p map[string]any) (any, error) {
			data, err := c.ListResourceNames(ctx, ParseGetParams(p))
			if err != nil {
				return nil, err
			}
			return wrapAsFinding(accountID, data)
		}
	}
	if k != nil {
		hs["kubectl_command_executor"] = func(ctx context.Context, p map[string]any) (any, error) {
			cmd := strParam(p, "command")
			data, err := k.Run(ctx, cmd)
			if err != nil {
				return nil, err
			}
			return wrapAsFinding(accountID, data)
		}
	}
	return hs
}

// wrapAsFinding marshals payload into a single JsonBlock and returns the
// Finding envelope. Errors here are unreachable in practice
// (json.Marshal of a kube payload that already round-tripped through
// client-go), but we surface them rather than swallowing.
func wrapAsFinding(accountID string, payload any) (any, error) {
	block, err := enrichers.JSONBlock(payload)
	if err != nil {
		return nil, err
	}
	return enrichers.FindingResponse(accountID, uuid.Nil, block)
}
