package servicemap

import "math"

// render walks the world and emits the Application list in the wire shape
// the api-server consumer expects. Apps without any edges (neither
// upstream nor downstream) are dropped (`apps_used` filter).
func render(w *world) []Application {
	out := make([]Application, 0, len(w.applications))
	used := map[string]bool{}

	// Build inverse-edge index for downstream lookup (dst → src list).
	downstreams := map[string]map[string]struct{}{}
	for srcK, dsts := range w.edges {
		for dstK := range dsts {
			if downstreams[dstK] == nil {
				downstreams[dstK] = map[string]struct{}{}
			}
			downstreams[dstK][srcK] = struct{}{}
		}
	}

	for k, a := range w.applications {
		app := Application{
			ID:               a.id,
			Labels:           a.labels,
			Status:           StatusUnknown,
			Indicators:       []any{},
			Upstreams:        []UpstreamLink{},
			Downstreams:      []DownstreamLink{},
			Type:             []string{},
			Instances:        []Instance{},
			DesiredInstances: a.desired,
			IsHealthy:        true,
		}
		// Failed-instance count.
		for _, inst := range a.instances {
			app.Instances = append(app.Instances, inst)
			if inst.IsFailed {
				app.FailedInstances++
			}
		}

		// Container stats roll-up.
		if s, ok := w.containerStats[k]; ok {
			app.OOMKills = saneInt(s.oomKills)
			app.Restarts = saneInt(s.restarts)
			app.CPUThrottlingTime = saneFloat(s.cpuThrottlingTime)
			app.VolumeSize = saneFloat(s.volumeSize)
			app.VolumeUsed = saneFloat(s.volumeUsed)
		}

		// Upstream edges out of this app. Id is a STRING in
		// `namespace:kind:name` shape — see UpstreamLink doc + comment in
		// types.go for why this differs from DownstreamLink/Application.Id.
		if dsts, ok := w.edges[k]; ok {
			for dstK, la := range dsts {
				dst, ok := w.applications[dstK]
				if !ok {
					continue
				}
				link := UpstreamLink{
					ID:            idToText(dst.id),
					Status:        StatusUnknown,
					Stats:         []string{},
					Weight:        saneFloat(la.requests),
					Latency:       saneFloat(la.latency),
					RequestCount:  saneFloat(la.requests),
					FailureCount:  saneFloat(la.failures),
					Protocol:      protocolOrDefault(la.protocol),
					BytesSent:     saneFloat(la.bytesSent),
					BytesReceived: saneFloat(la.bytesRecv),
				}
				app.Upstreams = append(app.Upstreams, link)
				used[k] = true
				used[dstK] = true
			}
		}
		// Downstream pointers (reverse edges). Id stays an object here.
		if srcs, ok := downstreams[k]; ok {
			for srcK := range srcs {
				src, ok := w.applications[srcK]
				if !ok {
					continue
				}
				app.Downstreams = append(app.Downstreams, DownstreamLink{
					ID:       src.id,
					Status:   StatusUnknown,
					Stats:    []string{},
					Protocol: "Unknown",
				})
				used[k] = true
				used[srcK] = true
			}
		}

		// Health computation.
		if app.CPUThrottlingTime > 0 {
			app.IsHealthy = false
			app.HealthReason = "CPUThrottling"
		}
		if app.OOMKills > 0 {
			app.IsHealthy = false
			app.HealthReason = "OOMKills"
		}
		if app.Restarts > 0 {
			app.IsHealthy = false
			app.HealthReason = "Restarts"
		}
		if app.FailedInstances > 0 {
			app.IsHealthy = false
			app.HealthReason = "FailedInstances"
		}

		out = append(out, app)
	}

	// Drop apps with no edges.
	filtered := make([]Application, 0, len(out))
	for _, a := range out {
		k := appKey(a.ID)
		if used[k] {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

func saneFloat(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func saneInt(v float64) int {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return int(v)
}

func protocolOrDefault(p string) string {
	if p == "" {
		return "Unknown"
	}
	return p
}
