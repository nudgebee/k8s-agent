package telemetry

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// node is a tiny builder for corev1.Node used across the table.
func node(name, providerID, kubeletVersion string, labels map[string]string) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{KubeletVersion: kubeletVersion},
		},
	}
}

// startIMDSMock spins up an httptest server that mimics IMDSv2: PUT /token
// returns the token, GET /document returns an instance-identity-document JSON
// containing accountId. tokenStatus / docStatus override the response codes
// for failure-path tests.
func startIMDSMock(t *testing.T, accountID string, tokenStatus, docStatus int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if tokenStatus != 0 && tokenStatus >= 400 {
			http.Error(w, "boom", tokenStatus)
			return
		}
		_, _ = w.Write([]byte("test-token"))
	})
	mux.HandleFunc("/document", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") == "" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		if docStatus != 0 && docStatus >= 400 {
			http.Error(w, "boom", docStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accountId":"` + accountID + `","region":"us-east-1"}`))
	})
	srv := httptest.NewServer(mux)

	origToken, origDoc := imdsTokenURL, imdsDocumentURL
	imdsTokenURL = srv.URL + "/token"
	imdsDocumentURL = srv.URL + "/document"
	t.Cleanup(func() {
		srv.Close()
		imdsTokenURL = origToken
		imdsDocumentURL = origDoc
	})
}

func TestDetectProvider(t *testing.T) {
	type imdsCfg struct {
		account     string
		tokenStatus int // 0 → success
		docStatus   int // 0 → success
	}

	cases := []struct {
		name  string
		nodes []corev1.Node
		imds  *imdsCfg // non-nil → start mock
		want  ProviderInfo
	}{
		{
			name: "EKS with IMDS",
			nodes: []corev1.Node{node("ip-10-0-1-2", "aws:///us-east-1a/i-abc",
				"v1.28.2-eks-abc1234", map[string]string{
					"topology.kubernetes.io/region": "us-east-1",
					"topology.kubernetes.io/zone":   "us-east-1a",
				})},
			imds: &imdsCfg{account: "123456789012"},
			want: ProviderInfo{
				Provider: providerEKS, AccountNumber: "123456789012",
				Region: "us-east-1", Zone: "us-east-1a",
			},
		},
		{
			name: "EKS without IMDS (Fargate / hop-limit)",
			nodes: []corev1.Node{node("ip-10-0-1-2", "aws:///us-east-1a/i-abc",
				"v1.28.2-eks-abc1234", map[string]string{
					"topology.kubernetes.io/region": "us-east-1",
					"topology.kubernetes.io/zone":   "us-east-1a",
				})},
			imds: &imdsCfg{tokenStatus: http.StatusUnauthorized},
			want: ProviderInfo{
				Provider: providerEKS, AccountNumber: "",
				Region: "us-east-1", Zone: "us-east-1a",
			},
		},
		{
			name: "GKE",
			nodes: []corev1.Node{node("gke-default-pool-1", "gce://my-project/us-central1-a/instance-1",
				"v1.27.3-gke.100", map[string]string{
					"topology.kubernetes.io/region": "us-central1",
					"topology.kubernetes.io/zone":   "us-central1-a",
				})},
			want: ProviderInfo{
				Provider: providerGKE, AccountNumber: "my-project", ProjectID: "my-project",
				Region: "us-central1", Zone: "us-central1-a",
			},
		},
		{
			name: "AKS",
			nodes: []corev1.Node{node("aks-nodepool-1",
				"azure:///subscriptions/SUB-123/resourceGroups/MC_rg/providers/Microsoft.Compute/virtualMachineScaleSets/aks-vmss-1/virtualMachines/0",
				"v1.27.3", map[string]string{
					"topology.kubernetes.io/region": "westeurope",
					"topology.kubernetes.io/zone":   "westeurope-1",
				})},
			want: ProviderInfo{
				Provider: providerAKS, AccountNumber: "SUB-123", ResourceGroup: "MC_rg",
				Region: "westeurope", Zone: "westeurope-1",
			},
		},
		{
			name: "Kind via hostname",
			nodes: []corev1.Node{node("kind-control-plane", "", "v1.27.0", map[string]string{
				"kubernetes.io/hostname": "kind-control-plane",
			})},
			want: ProviderInfo{Provider: providerKind},
		},
		{
			name: "RancherDesktop via hostname",
			nodes: []corev1.Node{node("lima", "", "v1.27.0", map[string]string{
				"kubernetes.io/hostname": "lima-rancher-desktop",
			})},
			want: ProviderInfo{Provider: providerRancherDesktop},
		},
		{
			name: "Minikube via label",
			nodes: []corev1.Node{node("minikube", "", "v1.27.0", map[string]string{
				"minikube.k8s.io/name": "minikube",
			})},
			want: ProviderInfo{Provider: providerMinikube},
		},
		{
			name: "Kops via label",
			nodes: []corev1.Node{node("ip-10-0-0-1", "", "v1.27.0", map[string]string{
				"kops.k8s.io/instancegroup": "nodes",
			})},
			want: ProviderInfo{Provider: providerKops},
		},
		{
			name: "DigitalOcean via label",
			nodes: []corev1.Node{node("do-node-1", "", "v1.27.0", map[string]string{
				"doks.digitalocean.com/version": "1.27.0",
			})},
			want: ProviderInfo{Provider: providerDigitalOcean},
		},
		{
			name: "Kapsule via label",
			nodes: []corev1.Node{node("scw-node-1", "", "v1.27.0", map[string]string{
				"k8s.scaleway.com/kapsule": "1",
			})},
			want: ProviderInfo{Provider: providerKapsule},
		},
		{
			name: "Civo via label",
			nodes: []corev1.Node{node("civo-node-1", "", "v1.27.0", map[string]string{
				"kubernetes.civo.com/civo-node-size": "g3.k3s.small",
			})},
			want: ProviderInfo{Provider: providerCivo},
		},
		{
			name:  "Bare-metal — no providerID, no labels",
			nodes: []corev1.Node{node("bare", "", "v1.27.0", nil)},
			want:  ProviderInfo{Provider: providerUnknown},
		},
		{
			name:  "No nodes",
			nodes: nil,
			want:  ProviderInfo{},
		},
		{
			// Check order: AKS (substring "aks") runs before kind
			// (substring "kind"), so a providerID containing both
			// resolves to AKS.
			name: "AKS-providerID containing 'kind' resolves to AKS",
			nodes: []corev1.Node{node("aks-kind-1",
				"azure:///subscriptions/S/resourceGroups/aks-kindof/providers/...",
				"v1.27.0", nil)},
			want: ProviderInfo{Provider: providerAKS, AccountNumber: "S", ResourceGroup: "aks-kindof"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.imds != nil {
				startIMDSMock(t, tc.imds.account, tc.imds.tokenStatus, tc.imds.docStatus)
			}
			objs := make([]runtime.Object, 0, len(tc.nodes))
			for i := range tc.nodes {
				n := tc.nodes[i]
				objs = append(objs, &n)
			}
			cs := fake.NewClientset(objs...)

			got := DetectProvider(context.Background(), cs, nil)
			if got != tc.want {
				t.Errorf("DetectProvider = %+v\n          want = %+v", got, tc.want)
			}
		})
	}
}

func TestDetectProvider_NodeListError(t *testing.T) {
	cs := fake.NewClientset()
	cs.PrependReactor("list", "nodes", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	got := DetectProvider(context.Background(), cs, nil)
	if got != (ProviderInfo{}) {
		t.Errorf("expected zero-value ProviderInfo on list error, got %+v", got)
	}
}

func TestDetectProvider_NilClientset(t *testing.T) {
	got := DetectProvider(context.Background(), nil, nil)
	if got != (ProviderInfo{}) {
		t.Errorf("expected zero-value ProviderInfo on nil clientset, got %+v", got)
	}
}

// TestIMDSAccountID_FailureModes covers the explicit fail-silent paths.
func TestIMDSAccountID_FailureModes(t *testing.T) {
	t.Run("token endpoint 5xx", func(t *testing.T) {
		startIMDSMock(t, "", http.StatusInternalServerError, 0)
		if got := imdsAccountID(context.Background()); got != "" {
			t.Errorf("expected empty on token 500, got %q", got)
		}
	})

	t.Run("document endpoint 5xx", func(t *testing.T) {
		startIMDSMock(t, "", 0, http.StatusInternalServerError)
		if got := imdsAccountID(context.Background()); got != "" {
			t.Errorf("expected empty on document 500, got %q", got)
		}
	})

	t.Run("unreachable host", func(t *testing.T) {
		origToken, origDoc := imdsTokenURL, imdsDocumentURL
		// 127.0.0.1:1 — connect refuses immediately, well under the 2s timeout.
		imdsTokenURL = "http://127.0.0.1:1/token"
		imdsDocumentURL = "http://127.0.0.1:1/document"
		t.Cleanup(func() {
			imdsTokenURL = origToken
			imdsDocumentURL = origDoc
		})
		if got := imdsAccountID(context.Background()); got != "" {
			t.Errorf("expected empty on unreachable host, got %q", got)
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("token-xyz"))
		})
		mux.HandleFunc("/document", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		origToken, origDoc := imdsTokenURL, imdsDocumentURL
		imdsTokenURL = srv.URL + "/token"
		imdsDocumentURL = srv.URL + "/document"
		t.Cleanup(func() {
			imdsTokenURL = origToken
			imdsDocumentURL = origDoc
		})
		if got := imdsAccountID(context.Background()); got != "" {
			t.Errorf("expected empty on malformed json, got %q", got)
		}
	})

	t.Run("happy path strips whitespace token", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("  token-xyz\n"))
		})
		mux.HandleFunc("/document", func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("X-aws-ec2-metadata-token"); strings.ContainsAny(got, " \n") || got != "token-xyz" {
				http.Error(w, "bad token: "+got, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"accountId":"42"}`))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		origToken, origDoc := imdsTokenURL, imdsDocumentURL
		imdsTokenURL = srv.URL + "/token"
		imdsDocumentURL = srv.URL + "/document"
		t.Cleanup(func() {
			imdsTokenURL = origToken
			imdsDocumentURL = origDoc
		})
		if got := imdsAccountID(context.Background()); got != "42" {
			t.Errorf("imdsAccountID = %q; want 42", got)
		}
	})
}
