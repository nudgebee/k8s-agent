package servicemap

// world is the in-memory state the builder accumulates. We hold only
// what the renderer needs.
type world struct {
	// applications keyed by ApplicationID.toText() (kind/namespace/name)
	applications map[string]*application

	// pod IP → app key (built from kube_pod_info)
	podIPToApp map[string]string

	// service ClusterIP → app key (built from kube_service_info; service IPs
	// also alias to the workload they front)
	serviceIPToApp map[string]string

	// per-application container stats (latest values)
	containerStats map[string]*containerSums

	// edges: srcAppKey → dstAppKey → linkAccum
	edges map[string]map[string]*linkAccum
}

type application struct {
	id        ApplicationID
	labels    map[string]string
	instances map[string]Instance // keyed by pod name
	desired   int
}

type containerSums struct {
	oomKills          float64
	restarts          float64
	cpuThrottlingTime float64
	volumeSize        float64
	volumeUsed        float64
}

type linkAccum struct {
	requests    float64
	failures    float64
	latency     float64
	bytesSent   float64
	bytesRecv   float64
	protocol    string
	hasRequests bool
}

func newWorld() *world {
	return &world{
		applications:   map[string]*application{},
		podIPToApp:     map[string]string{},
		serviceIPToApp: map[string]string{},
		containerStats: map[string]*containerSums{},
		edges:          map[string]map[string]*linkAccum{},
	}
}

// appKey is the canonical map key for an application
// (kind/namespace/name).
func appKey(id ApplicationID) string {
	return id.Kind + "/" + id.Namespace + "/" + id.Name
}

// upsertApp returns the application slot for id, creating it if needed.
func (w *world) upsertApp(id ApplicationID) *application {
	k := appKey(id)
	a, ok := w.applications[k]
	if !ok {
		a = &application{
			id:        id,
			labels:    map[string]string{},
			instances: map[string]Instance{},
		}
		w.applications[k] = a
	}
	return a
}

// containerStatsFor returns the per-app stats accumulator.
func (w *world) containerStatsFor(appK string) *containerSums {
	s, ok := w.containerStats[appK]
	if !ok {
		s = &containerSums{}
		w.containerStats[appK] = s
	}
	return s
}

// addEdge records a connection from src to dst. Subsequent additions for
// the same pair accumulate into the linkAccum.
func (w *world) addEdge(srcK, dstK string) *linkAccum {
	dsts, ok := w.edges[srcK]
	if !ok {
		dsts = map[string]*linkAccum{}
		w.edges[srcK] = dsts
	}
	la, ok := dsts[dstK]
	if !ok {
		la = &linkAccum{}
		dsts[dstK] = la
	}
	return la
}

// resolveAppByIP returns the app key (or "") for a connection-IP label.
// Pod IPs win; Service ClusterIPs are tried second.
func (w *world) resolveAppByIP(ip string) string {
	if ip == "" {
		return ""
	}
	if k, ok := w.podIPToApp[ip]; ok {
		return k
	}
	return w.serviceIPToApp[ip]
}
