package servicemap

// Wire types — match the Application/Link/Instance shapes the
// render_service_map step emits, so the api-server consumer can keep
// its existing parser (services/application/trace_service_map_test.go
// validates exactly these field names).

// Status mirrors the Status int enum.
type Status int

const (
	StatusUnknown Status = 0
	StatusOK      Status = 1
	StatusWarning Status = 2
	StatusFailed  Status = 3
)

// ApplicationID identifies an application across the topology. The "name"
// is typically the workload name; "kind" is Deployment/StatefulSet/etc.
type ApplicationID struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}

// Instance represents a single Pod in an application's replica set.
type Instance struct {
	ID       ApplicationID `json:"id"`
	IsFailed bool          `json:"is_failed"`
}

// UpstreamLink is one outgoing edge — Application points at the apps it
// CALLS. Id is emitted as a STRING in `namespace:kind:name` shape (the
// dict key from `to_text()`), not an object. The UI parses
// `u.Id.split(':')` directly, and the backend's UpstreamLink also keeps
// `Id` as a string. Emitting the object form here would crash the UI
// with `Id.split is not a function`.
type UpstreamLink struct {
	ID            string   `json:"Id"`
	Status        Status   `json:"Status"`
	Stats         []string `json:"Stats"`
	Weight        float64  `json:"Weight"`
	Latency       float64  `json:"Latency"`
	RequestCount  float64  `json:"RequestCount"`
	FailureCount  float64  `json:"FailureCount"`
	Protocol      string   `json:"Protocol"`
	BytesSent     float64  `json:"BytesSent"`
	BytesReceived float64  `json:"BytesReceived"`
}

// DownstreamLink is one incoming edge — apps that CALL this Application.
// Id is emitted as an ApplicationId OBJECT (`{namespace, kind, name}`)
// here — different from UpstreamLink. The backend's DownstreamLink
// matches this shape.
type DownstreamLink struct {
	ID            ApplicationID `json:"Id"`
	Status        Status        `json:"Status"`
	Stats         []string      `json:"Stats"`
	Weight        float64       `json:"Weight"`
	Latency       float64       `json:"Latency"`
	RequestCount  float64       `json:"RequestCount"`
	FailureCount  float64       `json:"FailureCount"`
	Protocol      string        `json:"Protocol"`
	BytesSent     float64       `json:"BytesSent"`
	BytesReceived float64       `json:"BytesReceived"`
}

// idToText returns the wire-shape string for an UpstreamLink.Id:
// `namespace:kind:name`.
func idToText(id ApplicationID) string {
	return id.Namespace + ":" + id.Kind + ":" + id.Name
}

// Application is the rendered topology node.
type Application struct {
	ID                ApplicationID     `json:"Id"`
	Category          string            `json:"Category"`
	Labels            map[string]string `json:"Labels"`
	Status            Status            `json:"Status"`
	Indicators        []any             `json:"Indicators"`
	Upstreams         []UpstreamLink    `json:"Upstreams"`
	Downstreams       []DownstreamLink  `json:"Downstreams"`
	Instances         []Instance        `json:"Instances"`
	Type              []string          `json:"Type"`
	DesiredInstances  int               `json:"DesiredInstances"`
	FailedInstances   int               `json:"FailedInstances"`
	OOMKills          int               `json:"OOMKills"`
	Restarts          int               `json:"Restarts"`
	CPUThrottlingTime float64           `json:"CPUThrottlingTime"`
	VolumeSize        float64           `json:"VolumeSize"`
	VolumeUsed        float64           `json:"VolumeUsed"`
	IsHealthy         bool              `json:"IsHealthy"`
	HealthReason      string            `json:"HealthReason"`
}

// FilterParams are the action_params accepted by service_map.
//
//	workload_name      - if set, scopes the query to one workload
//	workload_namespace - similar
//	r_start_time / r_end_time - RFC3339-like, or empty for "now - duration"
//	duration  - minutes (default 1440 = 24h)
type FilterParams struct {
	WorkloadName      string `json:"workload_name"`
	WorkloadNamespace string `json:"workload_namespace"`
	StartTime         string `json:"r_start_time"`
	EndTime           string `json:"r_end_time"`
	Duration          int    `json:"duration"`
}
