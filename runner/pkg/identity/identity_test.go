package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// tokenReactor answers TokenRequests with "tok-<sa>-<n>" and the given TTL.
func tokenReactor(cs *fake.Clientset, ttl time.Duration, fail *atomic.Int32) {
	var n atomic.Int32
	cs.PrependReactor("create", "serviceaccounts", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetSubresource() != "token" {
			return false, nil, nil
		}
		if fail != nil && fail.Load() > 0 {
			fail.Add(-1)
			return true, nil, errors.New("api unavailable")
		}
		c := a.(k8stesting.CreateActionImpl)
		name := c.Name
		i := n.Add(1)
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{
			Token:               "tok-" + name + "-" + strconv.Itoa(int(i)),
			ExpirationTimestamp: metav1.NewTime(time.Now().Add(ttl)),
		}}, nil
	})
}

func instantSleep(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }

func TestTokenSource_MintsAndRetries(t *testing.T) {
	cs := fake.NewClientset()
	var fail atomic.Int32
	fail.Store(2) // two failures, then success
	tokenReactor(cs, 10*time.Minute, &fail)
	ts := NewTokenSource(cs, "nb", "reader-sa", nil)
	ts.sleep = instantSleep
	if _, err := ts.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("before Start: err = %v, want ErrNoToken", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ts.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	tok, err := ts.Token()
	if err != nil || !strings.HasPrefix(tok, "tok-reader-sa-") {
		t.Fatalf("Token() = %q, %v", tok, err)
	}
	if got := fail.Load(); got != 0 {
		t.Errorf("expected both failures consumed, %d left", got)
	}
}

func TestTokenSource_ExpiredTokenFailsClosed(t *testing.T) {
	cs := fake.NewClientset()
	tokenReactor(cs, 10*time.Minute, nil)
	ts := NewTokenSource(cs, "nb", "sa", nil)
	if err := ts.mint(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts.now = func() time.Time { return time.Now().Add(time.Hour) }
	if _, err := ts.Token(); !errors.Is(err, ErrNoToken) {
		t.Fatalf("expired token: err = %v, want ErrNoToken", err)
	}
}

func TestTokenSource_RefreshAt80Percent(t *testing.T) {
	cs := fake.NewClientset()
	tokenReactor(cs, 10*time.Minute, nil)
	ts := NewTokenSource(cs, "nb", "sa", nil)
	now := time.Now()
	ts.now = func() time.Time { return now }
	if err := ts.mint(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := ts.refreshAt.Sub(now)
	if got < 7*time.Minute || got > 9*time.Minute {
		t.Errorf("refresh after %v, want ~8m", got)
	}
}

func TestTokenConfig_ClearsEveryOtherCredential(t *testing.T) {
	base := &rest.Config{
		Host:            "https://example.invalid",
		BearerToken:     "runner-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		Username:        "u", Password: "p",
		Impersonate:     rest.ImpersonationConfig{UserName: "someone"},
		TLSClientConfig: rest.TLSClientConfig{CertData: []byte("c"), KeyData: []byte("k"), CertFile: "f", KeyFile: "g"},
		ExecProvider:    nil,
	}
	cfg := TokenConfig(base, NewTokenSource(fake.NewClientset(), "nb", "sa", nil))
	if cfg.BearerToken != "" || cfg.BearerTokenFile != "" || cfg.Username != "" || cfg.Password != "" ||
		cfg.Impersonate.UserName != "" || cfg.CertData != nil || cfg.KeyData != nil || cfg.CertFile != "" || cfg.KeyFile != "" {
		t.Fatalf("credential left on token config: %+v", cfg)
	}
	if base.BearerToken != "runner-token" {
		t.Fatal("base config was modified")
	}
}

// A request through a token config carries only the identity token, and
// fails before it is sent when no token is available.
func TestTokenConfig_RequestCarriesOnlyIdentityToken(t *testing.T) {
	var gotAuth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NamespaceList","apiVersion":"v1","items":[]}`))
	}))
	defer srv.Close()

	ts := NewTokenSource(nil, "nb", "sa", nil)
	base := &rest.Config{Host: srv.URL, BearerToken: "runner-token"}
	cs, err := kubernetes.NewForConfig(TokenConfig(base, ts))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{}); err == nil {
		t.Fatal("request without a token must fail")
	}
	if v := gotAuth.Load(); v != nil {
		t.Fatalf("request reached the server without a token (Authorization %q)", v)
	}

	ts.mu.Lock()
	ts.token, ts.expires = "identity-token", time.Now().Add(time.Minute)
	ts.mu.Unlock()
	if _, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := gotAuth.Load(); got != "Bearer identity-token" {
		t.Fatalf("Authorization = %q, want the identity token", got)
	}
}

// fakeIdentity returns a Clients backed by a fake clientset whose
// SelfSubjectReview and SelfSubjectAccessReview answers are controlled.
func fakeIdentity(name, user string, ssrServed bool, allow func(authorizationv1.ResourceAttributes) bool) *Clients {
	return fakeIdentityErr(name, user, ssrServed, nil, allow)
}

// fakeIdentityErr is fakeIdentity with the error a cluster that does not serve
// SelfSubjectReview answers (default 404).
func fakeIdentityErr(name, user string, ssrServed bool, ssrErr error, allow func(authorizationv1.ResourceAttributes) bool) *Clients {
	cs := fake.NewClientset()
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		if !ssrServed {
			if ssrErr != nil {
				return true, nil, ssrErr
			}
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "")
		}
		return true, &authenticationv1.SelfSubjectReview{Status: authenticationv1.SelfSubjectReviewStatus{UserInfo: authenticationv1.UserInfo{Username: user}}}, nil
	})
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		r := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
		r.Status.Allowed = allow(*r.Spec.ResourceAttributes)
		return true, r, nil
	})
	ts := NewTokenSource(nil, "nb", name, nil)
	ts.token, ts.expires = "t", time.Now().Add(time.Hour)
	return &Clients{Name: name, ServiceAccount: Username("nb", "t-"+name), SAName: "t-" + name, Typed: cs, Tokens: ts}
}

var probes = []Resource{{Resource: "pods"}, {Group: "apps", Resource: "deployments"}}

func readerRights(a authorizationv1.ResourceAttributes) bool {
	return a.Verb == "list" || a.Verb == "watch" || a.Verb == "get"
}

func runnerRights(authorizationv1.ResourceAttributes) bool { return true }

func TestCheck_IdentityProven(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, Username("nb", "t-reader"), true, readerRights)
	if st := s.Check(context.Background(), probes, nil); !st.OK {
		t.Fatalf("reader check: %+v", st)
	}
	if err := s.Usable(NameReader); err != nil {
		t.Fatal(err)
	}
	if err := s.Usable(NameWriter); !errors.Is(err, ErrDisabled) {
		t.Fatalf("writer usable = %v, want ErrDisabled", err)
	}
}

func TestCheck_WrongUserFails(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	// Wired by mistake to the runner's credential.
	s.Reader = fakeIdentity(NameReader, Username("nb", "t-runner-service-account"), true, runnerRights)
	st := s.Check(context.Background(), probes, nil)
	if st.OK || !strings.Contains(st.Reason, "authenticates as") {
		t.Fatalf("status = %+v, want identity mismatch", st)
	}
}

func TestCheck_NegativeProbeWhenSelfSubjectReviewNotServed(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, "", false, readerRights)
	if st := s.Check(context.Background(), probes, nil); !st.OK {
		t.Fatalf("real reader on an old cluster: %+v", st)
	}
	s.Reader = fakeIdentity(NameReader, "", false, runnerRights)
	if st := s.Check(context.Background(), probes, nil); st.OK || !strings.Contains(st.Reason, "negative probe allowed") {
		t.Fatalf("runner credential on an old cluster: %+v, want negative probe failure", st)
	}
}

func TestCheck_WriterNegativeProbeInconclusiveIsWarning(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, "", false, readerRights)
	s.Writer = fakeIdentity(NameWriter, "", false, runnerRights)
	s.Check(context.Background(), probes, nil)
	st := s.Statuses()[NameWriter]
	if !st.OK || st.Warning == "" {
		t.Fatalf("writer status = %+v, want OK with warning", st)
	}
}

func TestCheck_MissingReaderPermissionNamed(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, Username("nb", "t-reader"), true, func(a authorizationv1.ResourceAttributes) bool {
		return a.Resource != "deployments" || a.Verb != "watch"
	})
	st := s.Check(context.Background(), probes, nil)
	if st.OK || !strings.Contains(st.Reason, "watch deployments.apps") {
		t.Fatalf("status = %+v, want missing watch deployments.apps", st)
	}
}

// A writer failure never fails the reader.
func TestCheck_WriterFailureLeavesReaderOK(t *testing.T) {
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, Username("nb", "t-reader"), true, readerRights)
	s.Writer = fakeIdentity(NameWriter, Username("nb", "t-writer"), true, readerRights) // cannot patch
	if st := s.Check(context.Background(), probes, nil); !st.OK {
		t.Fatalf("reader: %+v", st)
	}
	if err := s.Usable(NameWriter); err == nil || !strings.Contains(err.Error(), "patch deployments") {
		t.Fatalf("writer usable = %v, want missing patch", err)
	}
}

func TestLegacy_EveryIdentityIsBase(t *testing.T) {
	cs := fake.NewClientset()
	s := Legacy(cs, nil, &rest.Config{})
	if s.Reader != s.Base || s.Writer != s.Base || s.Exec != s.Base {
		t.Fatal("legacy identities must all be the base client")
	}
	for _, n := range []string{NameReader, NameExec, NameWriter} {
		if err := s.Usable(n); err != nil {
			t.Fatalf("legacy %s: %v", n, err)
		}
	}
	if len(s.All()) != 1 {
		t.Fatalf("All() = %d entries, want 1", len(s.All()))
	}
}

// Before 1.28 a cluster that does not serve SelfSubjectReview answers 403
// (authorization runs before the resource is resolved); the negative probe
// must still prove the identity.
func TestCheck_ForbiddenSelfSubjectReviewUsesNegativeProbe(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "", errors.New("no"))
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentityErr(NameReader, "", false, forbidden, readerRights)
	if st := s.Check(context.Background(), probes, nil); !st.OK {
		t.Fatalf("reader on a 1.26-style cluster: %+v", st)
	}
	s.Reader = fakeIdentityErr(NameReader, "", false, forbidden, runnerRights)
	if st := s.Check(context.Background(), probes, nil); st.OK {
		t.Fatal("runner credential on a 1.26-style cluster must fail the negative probe")
	}
}

// A read-only runner (no write rights) still holds the token Role, so the
// negative probe catches a reader wired to the runner's credential there too.
func TestCheck_NegativeProbeCatchesReadOnlyRunner(t *testing.T) {
	readOnlyRunner := func(a authorizationv1.ResourceAttributes) bool {
		return readerRights(a) || (a.Verb == "create" && a.Resource == "serviceaccounts" && a.Subresource == "token")
	}
	s := &Set{Split: true, Namespace: "nb", status: map[string]Status{}}
	s.Reader = fakeIdentity(NameReader, "", false, readOnlyRunner)
	if st := s.Check(context.Background(), probes, nil); st.OK {
		t.Fatal("reader going out as a read-only runner must fail the negative probe")
	}
}
