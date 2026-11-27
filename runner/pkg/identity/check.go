package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Resource names one API resource the reader must be able to list and watch.
type Resource struct {
	Group, Resource string
}

func (r Resource) String() string {
	if r.Group == "" {
		return r.Resource
	}
	return r.Resource + "." + r.Group
}

// Status is the outcome of the last check of one identity.
type Status struct {
	OK        bool      `json:"ok"`
	Reason    string    `json:"reason,omitempty"`
	Warning   string    `json:"warning,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// ErrDisabled is returned by Usable for an identity the chart turned off.
var ErrDisabled = errors.New("disabled on this agent")

// Usable reports whether calls may use the named identity: nil when its last
// check passed (or in legacy mode), otherwise an error naming the reason.
func (s *Set) Usable(name string) error {
	if !s.Split {
		return nil
	}
	if s.clients(name) == nil {
		return fmt.Errorf("%s identity: %w", name, ErrDisabled)
	}
	s.mu.RLock()
	st, ok := s.status[name]
	s.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%s identity: not checked yet", name)
	}
	if !st.OK {
		return fmt.Errorf("%s identity: %s", name, st.Reason)
	}
	return nil
}

// Statuses returns a copy of the last check results, by identity name.
func (s *Set) Statuses() map[string]Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Status, len(s.status))
	for k, v := range s.status {
		out[k] = v
	}
	return out
}

func (s *Set) clients(name string) *Clients {
	switch name {
	case NameBase:
		return s.Base
	case NameReader:
		return s.Reader
	case NameExec:
		return s.Exec
	case NameWriter:
		return s.Writer
	}
	return nil
}

func (s *Set) setStatus(name string, st Status) {
	s.mu.Lock()
	s.status[name] = st
	s.mu.Unlock()
}

// Check proves each split identity and records the result:
//
//  1. Who: SelfSubjectReview must return the identity's username. Where that
//     API is not served (before 1.27), a negative probe instead: a right the
//     runner has and the identity lacks must be denied, which proves the
//     request did not go out with the runner's credential.
//  2. What: the reader must list and watch every resource in readerProbes;
//     exec must create pods/exec; writer must patch deployments.
//
// It returns the reader's status; exec and writer failures are recorded but
// never fail the reader.
func (s *Set) Check(ctx context.Context, readerProbes []Resource, logger *slog.Logger) Status {
	if !s.Split {
		return Status{OK: true, CheckedAt: time.Now()}
	}
	if logger == nil {
		logger = slog.Default()
	}
	for _, c := range []*Clients{s.Reader, s.Exec, s.Writer} {
		if c == nil {
			continue
		}
		st := s.checkOne(ctx, c, readerProbes)
		s.setStatus(c.Name, st)
		switch {
		case !st.OK:
			logger.Error("identity check failed", "identity", c.Name, "service_account", c.ServiceAccount, "reason", st.Reason)
		case st.Warning != "":
			logger.Warn("identity check inconclusive", "identity", c.Name, "service_account", c.ServiceAccount, "warning", st.Warning)
		default:
			logger.Info("identity check passed", "identity", c.Name, "service_account", c.ServiceAccount)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status[NameReader]
}

func (s *Set) checkOne(ctx context.Context, c *Clients, readerProbes []Resource) Status {
	now := time.Now()
	if _, err := c.Tokens.Token(); err != nil {
		return Status{Reason: err.Error(), CheckedAt: now}
	}
	warning, err := s.proveIdentity(ctx, c)
	if err != nil {
		return Status{Reason: err.Error(), CheckedAt: now}
	}
	var missing []string
	switch c.Name {
	case NameReader:
		for _, r := range readerProbes {
			for _, verb := range []string{"list", "watch"} {
				ok, err := allowed(ctx, c.Typed, authorizationv1.ResourceAttributes{Verb: verb, Group: r.Group, Resource: r.Resource})
				if err != nil {
					return Status{Reason: err.Error(), CheckedAt: now}
				}
				if !ok {
					missing = append(missing, verb+" "+r.String())
				}
			}
		}
	case NameExec:
		ok, err := allowed(ctx, c.Typed, authorizationv1.ResourceAttributes{Verb: "create", Resource: "pods", Subresource: "exec"})
		if err != nil {
			return Status{Reason: err.Error(), CheckedAt: now}
		}
		if !ok {
			missing = append(missing, "create pods/exec")
		}
	case NameWriter:
		ok, err := allowed(ctx, c.Typed, authorizationv1.ResourceAttributes{Verb: "patch", Group: "apps", Resource: "deployments"})
		if err != nil {
			return Status{Reason: err.Error(), CheckedAt: now}
		}
		if !ok {
			missing = append(missing, "patch deployments.apps")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Status{Reason: "missing permissions: " + strings.Join(missing, ", "), Warning: warning, CheckedAt: now}
	}
	return Status{OK: true, Warning: warning, CheckedAt: now}
}

// proveIdentity returns an error when the client does not authenticate as
// c.ServiceAccount, and a warning when the fallback probe is inconclusive.
func (s *Set) proveIdentity(ctx context.Context, c *Clients) (warning string, err error) {
	ssr, err := c.Typed.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authenticationv1.SelfSubjectReview{}, metav1.CreateOptions{})
	switch {
	case err == nil:
		if got := ssr.Status.UserInfo.Username; got != c.ServiceAccount {
			return "", fmt.Errorf("authenticates as %q, want %q", got, c.ServiceAccount)
		}
		return "", nil
	case !apierrors.IsNotFound(err) && !apierrors.IsMethodNotSupported(err) && !apierrors.IsForbidden(err):
		return "", fmt.Errorf("self subject review: %w", err)
	}
	// SelfSubjectReview not served (404), or not granted: before 1.28 the
	// API server authorizes before it resolves the resource, so a cluster that
	// does not serve it answers 403 (system:basic-user grants it from 1.28).
	// Negative probe instead.
	// In split mode only the runner's own account may mint the reader's token
	// (the chart's identity-tokens Role), whatever else it may or may not do.
	probe := authorizationv1.ResourceAttributes{
		Verb: "create", Resource: "serviceaccounts", Subresource: "token",
		Namespace: s.Namespace, Name: s.Reader.SAName,
	}
	ok, err := allowed(ctx, c.Typed, probe)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if c.Name == NameWriter {
		// The writer's own rules (customClusterRoleRules with `*` resources)
		// may grant the probed right: inconclusive, not a failure.
		return "self subject review not served and the writer's own rules allow the negative probe (create serviceaccounts/token); identity not proven", nil
	}
	return "", fmt.Errorf("negative probe allowed (create serviceaccounts/token): requests may be going out as the runner")
}

func allowed(ctx context.Context, cs kubernetes.Interface, attrs authorizationv1.ResourceAttributes) (bool, error) {
	a := attrs
	review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &a}}
	res, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("access review %s %s: %w", attrs.Verb, attrs.Resource, err)
	}
	return res.Status.Allowed, nil
}

// failedRecheck is how soon a check is repeated while an identity fails.
const failedRecheck = 30 * time.Second

// Watch re-checks every interval (sooner while an identity fails) until ctx
// ends, so an identity fixed by a chart change or a transient API failure
// recovers without a restart.
func (s *Set) Watch(ctx context.Context, interval time.Duration, readerProbes []Resource, logger *slog.Logger, onChange func(reader Status)) {
	if !s.Split {
		return
	}
	prev := s.Statuses()[NameReader].OK
	for {
		wait := interval
		for _, st := range s.Statuses() {
			if !st.OK {
				wait = min(interval, failedRecheck)
			}
		}
		if !sleepCtx(ctx, wait) {
			return
		}
		st := s.Check(ctx, readerProbes, logger)
		if st.OK != prev && onChange != nil {
			onChange(st)
		}
		prev = st.OK
	}
}
