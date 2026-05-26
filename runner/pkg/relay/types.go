package relay

// Wire types shared with the legacy runner. Field names match the wire
// contract for action requests and sync responses.

// ExternalActionRequest is the inbound envelope from the relay.
type ExternalActionRequest struct {
	Body         ActionRequestBody `json:"body"`
	Signature    string            `json:"signature,omitempty"`
	PartialAuthA string            `json:"partial_auth_a,omitempty"`
	PartialAuthB string            `json:"partial_auth_b,omitempty"`
	RequestID    string            `json:"request_id,omitempty"`
	NoSinks      bool              `json:"no_sinks,omitempty"`
}

// ActionRequestBody is the inner payload that gets HMAC-signed.
type ActionRequestBody struct {
	AccountID    string         `json:"account_id,omitempty"`
	ClusterName  string         `json:"cluster_name,omitempty"`
	ActionName   string         `json:"action_name"`
	Timestamp    int64          `json:"timestamp"`
	ActionParams map[string]any `json:"action_params,omitempty"`
	Sinks        []string       `json:"sinks,omitempty"`
	Origin       string         `json:"origin,omitempty"`
}

// Response is the outbound envelope sent in reply to an action request.
type Response struct {
	Action     string `json:"action"` // always "response"
	RequestID  string `json:"request_id"`
	StatusCode int    `json:"status_code"`
	Data       any    `json:"data"`
	OutputType string `json:"output_type,omitempty"` // "actions" | "Grafana" | "Terminal"
}

// Greeting is sent on WS open. Carries agent version metadata the relay
// uses to populate the agent-version table.
type Greeting struct {
	Action         string `json:"action"` // "auth"
	Version        string `json:"version"`
	AgentVersion   string `json:"agent_version,omitempty"`
	AgentCommit    string `json:"agent_commit,omitempty"`
	AgentBuildTime string `json:"agent_build_time,omitempty"`
}
