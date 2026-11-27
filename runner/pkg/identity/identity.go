// Package identity gives the runner one Kubernetes client per identity:
//
//   - Base: the runner's own ServiceAccount. Release-namespace housekeeping
//     (script/scan pods, their ConfigMaps, own PrometheusRules) and minting
//     the tokens below.
//   - Reader: cluster reads (informers, typed reads, kubectl reads).
//   - Exec: pods/exec into customer pods (pod_bash_enricher, terminal).
//   - Writer: changes (mutate, rightsizing apply, kubectl writes, profiler).
//
// In split mode each non-base identity authenticates with a short-lived
// TokenRequest token for its own ServiceAccount, so the API server, not the
// runner's own classification, decides what a call may do, and the audit log
// names the identity. Legacy mode (a chart that predates split identities)
// gives every identity the base client, which is today's behaviour.
package identity

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Names of the identities, used in logs, status and errors.
const (
	NameBase   = "base"
	NameReader = "reader"
	NameExec   = "exec"
	NameWriter = "writer"
)

// Clients is one identity's view of the cluster.
type Clients struct {
	Name string
	// ServiceAccount is the identity's username
	// (system:serviceaccount:<ns>:<name>); empty for Base in legacy mode.
	ServiceAccount string
	Typed          kubernetes.Interface
	Dynamic        dynamic.Interface
	Rest           *rest.Config
	// Tokens is nil for Base.
	Tokens *TokenSource
	// SAName is the ServiceAccount's name (no prefix); empty for Base.
	SAName string
}

// Set holds the identities. Exec and Writer are nil when the chart disabled
// them (runner.exec.enabled / runner.changes.enabled false, or readOnly).
type Set struct {
	Split                      bool
	Namespace                  string
	Base, Reader, Exec, Writer *Clients

	mu     sync.RWMutex
	status map[string]Status
}

// Config names the ServiceAccounts of a split Set. ExecSA and WriterSA may be
// empty (identity disabled); ReaderSA is required.
type Config struct {
	Namespace                  string
	ReaderSA, ExecSA, WriterSA string
}

// Legacy returns a Set where every identity is the base client.
func Legacy(typed kubernetes.Interface, dyn dynamic.Interface, cfg *rest.Config) *Set {
	base := &Clients{Name: NameBase, Typed: typed, Dynamic: dyn, Rest: cfg}
	return &Set{Base: base, Reader: base, Exec: base, Writer: base, status: map[string]Status{}}
}

// NewSplit builds the per-identity clients. It does not mint tokens; call
// Start for that. base must be the runner's own config.
func NewSplit(base *rest.Config, baseTyped kubernetes.Interface, baseDyn dynamic.Interface, cfg Config, logger *slog.Logger) (*Set, error) {
	if cfg.Namespace == "" || cfg.ReaderSA == "" {
		return nil, fmt.Errorf("identity: split mode needs a namespace and a reader service account")
	}
	s := &Set{
		Split:     true,
		Namespace: cfg.Namespace,
		Base:      &Clients{Name: NameBase, Typed: baseTyped, Dynamic: baseDyn, Rest: base},
		status:    map[string]Status{},
	}
	var err error
	if s.Reader, err = newClients(NameReader, base, baseTyped, cfg.Namespace, cfg.ReaderSA, logger); err != nil {
		return nil, err
	}
	if cfg.ExecSA != "" {
		if s.Exec, err = newClients(NameExec, base, baseTyped, cfg.Namespace, cfg.ExecSA, logger); err != nil {
			return nil, err
		}
	}
	if cfg.WriterSA != "" {
		if s.Writer, err = newClients(NameWriter, base, baseTyped, cfg.Namespace, cfg.WriterSA, logger); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func newClients(name string, base *rest.Config, baseTyped kubernetes.Interface, ns, sa string, logger *slog.Logger) (*Clients, error) {
	ts := NewTokenSource(baseTyped, ns, sa, logger)
	cfg := TokenConfig(base, ts)
	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("identity: %s clientset: %w", name, err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("identity: %s dynamic client: %w", name, err)
	}
	return &Clients{
		Name: name, ServiceAccount: Username(ns, sa), SAName: sa,
		Typed: typed, Dynamic: dyn, Rest: cfg, Tokens: ts,
	}, nil
}

// Username is the API server's username for a ServiceAccount.
func Username(ns, sa string) string { return "system:serviceaccount:" + ns + ":" + sa }

// TokenConfig copies base and replaces every credential with ts: bearer
// token and token file, client certificates, basic auth, exec and auth
// providers, and impersonation are all cleared, so nothing but ts's token can
// authenticate a request built from the result.
func TokenConfig(base *rest.Config, ts *TokenSource) *rest.Config {
	cfg := rest.CopyConfig(base)
	cfg.BearerToken = ""
	cfg.BearerTokenFile = ""
	cfg.Username, cfg.Password = "", ""
	cfg.ExecProvider = nil
	cfg.AuthProvider = nil
	cfg.Impersonate = rest.ImpersonationConfig{}
	cfg.CertFile, cfg.KeyFile = "", ""
	cfg.CertData, cfg.KeyData = nil, nil
	prev := cfg.WrapTransport
	cfg.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if prev != nil {
			rt = prev(rt)
		}
		return &bearerTransport{ts: ts, next: rt}
	}
	return cfg
}

// Start begins minting and refreshing every split identity's token in the
// background; it never blocks. Use WaitTokens to wait for the first tokens.
func (s *Set) Start(ctx context.Context) {
	if !s.Split {
		return
	}
	for _, c := range []*Clients{s.Reader, s.Exec, s.Writer} {
		if c != nil {
			go func(c *Clients) { _ = c.Tokens.Start(ctx) }(c)
		}
	}
}

// WaitTokens waits until every enabled identity has a token, ctx ends, or
// timeout passes; it returns the first identity's error still outstanding.
func (s *Set) WaitTokens(ctx context.Context, timeout time.Duration) error {
	if !s.Split {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		var err error
		for _, c := range []*Clients{s.Reader, s.Exec, s.Writer} {
			if c == nil {
				continue
			}
			if _, e := c.Tokens.Token(); e != nil {
				err = fmt.Errorf("%s: %w", c.Name, e)
				break
			}
		}
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		if !sleepCtx(ctx, 200*time.Millisecond) {
			return ctx.Err()
		}
	}
}

// All returns the configured identities in a fixed order, skipping nil ones
// and duplicates (legacy mode).
func (s *Set) All() []*Clients {
	var out []*Clients
	seen := map[*Clients]bool{}
	for _, c := range []*Clients{s.Base, s.Reader, s.Exec, s.Writer} {
		if c != nil && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
