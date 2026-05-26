package servicemap

import (
	"testing"
)

func TestRender_DropsAppsWithNoEdges(t *testing.T) {
	w := newWorld()
	// Three apps: A connects to B; C is orphaned (no edges).
	a := ApplicationID{Name: "A", Kind: "Deployment", Namespace: "n"}
	b := ApplicationID{Name: "B", Kind: "Deployment", Namespace: "n"}
	c := ApplicationID{Name: "C", Kind: "Deployment", Namespace: "n"}
	w.upsertApp(a)
	w.upsertApp(b)
	w.upsertApp(c)
	la := w.addEdge(appKey(a), appKey(b))
	la.requests = 10
	la.protocol = "HTTP"

	apps := render(w)
	names := map[string]bool{}
	for _, app := range apps {
		names[app.ID.Name] = true
	}
	if !names["A"] || !names["B"] {
		t.Errorf("A and B should be in output: %v", names)
	}
	if names["C"] {
		t.Error("C had no edges and should be dropped")
	}
}

func TestRender_UpstreamLinkValues(t *testing.T) {
	w := newWorld()
	a := ApplicationID{Name: "A", Kind: "Deployment", Namespace: "n"}
	b := ApplicationID{Name: "B", Kind: "Deployment", Namespace: "n"}
	w.upsertApp(a)
	w.upsertApp(b)
	la := w.addEdge(appKey(a), appKey(b))
	la.requests = 100
	la.failures = 5
	la.latency = 0.25
	la.bytesSent = 1024
	la.bytesRecv = 2048
	la.protocol = "HTTP"

	apps := render(w)
	var upA *Application
	for i := range apps {
		if apps[i].ID.Name == "A" {
			upA = &apps[i]
		}
	}
	if upA == nil || len(upA.Upstreams) != 1 {
		t.Fatalf("A should have 1 upstream; apps=%+v", apps)
	}
	link := upA.Upstreams[0]
	if link.RequestCount != 100 || link.FailureCount != 5 || link.Latency != 0.25 {
		t.Errorf("link metrics: %+v", link)
	}
	if link.Protocol != "HTTP" || link.BytesSent != 1024 || link.BytesReceived != 2048 {
		t.Errorf("link transport: %+v", link)
	}
}

func TestRender_ReverseDownstreamPointers(t *testing.T) {
	w := newWorld()
	a := ApplicationID{Name: "A", Kind: "Deployment", Namespace: "n"}
	b := ApplicationID{Name: "B", Kind: "Deployment", Namespace: "n"}
	w.upsertApp(a)
	w.upsertApp(b)
	la := w.addEdge(appKey(a), appKey(b))
	la.requests = 10
	la.protocol = "HTTP"

	apps := render(w)
	var downB *Application
	for i := range apps {
		if apps[i].ID.Name == "B" {
			downB = &apps[i]
		}
	}
	if downB == nil || len(downB.Downstreams) != 1 || downB.Downstreams[0].ID.Name != "A" {
		t.Errorf("B should have downstream pointing back to A: %+v", downB)
	}
}

func TestRender_HealthFromContainerStats(t *testing.T) {
	w := newWorld()
	a := ApplicationID{Name: "A", Kind: "Deployment", Namespace: "n"}
	b := ApplicationID{Name: "B", Kind: "Deployment", Namespace: "n"}
	w.upsertApp(a)
	w.upsertApp(b)
	la := w.addEdge(appKey(a), appKey(b))
	la.requests = 1
	w.containerStatsFor(appKey(a)).oomKills = 2
	w.containerStatsFor(appKey(a)).restarts = 0

	apps := render(w)
	for _, app := range apps {
		if app.ID.Name == "A" {
			if app.OOMKills != 2 {
				t.Errorf("A.OOMKills = %d; want 2", app.OOMKills)
			}
			if app.IsHealthy {
				t.Error("A should be unhealthy due to OOMKills")
			}
			if app.HealthReason != "OOMKills" {
				t.Errorf("A.HealthReason = %q; want OOMKills", app.HealthReason)
			}
		}
	}
}

func TestSaneFloat(t *testing.T) {
	if saneFloat(1.5) != 1.5 {
		t.Error("saneFloat should pass through finite values")
	}
	// NaN and Inf return 0.
	nan := saneFloat(0.0 / nonZeroNonZero())
	if nan != 0 {
		t.Errorf("saneFloat(NaN) = %v; want 0", nan)
	}
}

// nonZeroNonZero produces a non-zero divisor for the NaN expression — Go
// compile-time blocks 0.0/0.0 directly.
func nonZeroNonZero() float64 {
	x := 0.0
	return x
}
