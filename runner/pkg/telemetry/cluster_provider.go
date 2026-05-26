package telemetry

// Cluster-provider detection: six-step provider classification chain
// followed by providerID parsing for region/zone/account/project/resource-group.
//
// Design notes:
//
//   - Detection runs once at startup (called from cmd/agent/main.go) and the
//     resulting ProviderInfo is cached in telemetry.Service for the agent's
//     lifetime. Region/zone/account/project are stable per cluster lifetime, so
//     periodic re-detection would just be wasted work + IMDS traffic.
//
//   - For AWS the providerID has no account number (just `aws:///<az>/<id>`)
//     so we hit IMDSv2 once. On Fargate, hop-limit-blocked, or non-AWS clusters
//     IMDS is unreachable; the helper returns "" and the backend skips the
//     empty value in its UPSERT.
//
//   - This file does not import kubernetes/fake — testability comes from the
//     `kubernetes.Interface` parameter on DetectProvider plus package-level
//     `var`s for the IMDS endpoints.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// ProviderInfo is the cached detection result, populated once at agent startup
// and read on every telemetry tick. Empty fields are treated as "unknown" by
type ProviderInfo struct {
	Provider      string
	AccountNumber string
	Region        string
	Zone          string
	ProjectID     string
	ResourceGroup string
}

// Cluster-provider type names (string-equivalent of the ClusterProviderType enum).
const (
	providerGKE            = "GKE"
	providerAKS            = "AKS"
	providerEKS            = "EKS"
	providerKind           = "Kind"
	providerMinikube       = "Minikube"
	providerRancherDesktop = "RancherDesktop"
	providerKapsule        = "Kapsule"
	providerKops           = "Kops"
	providerDigitalOcean   = "DigitalOcean"
	providerCivo           = "Civo"
	providerUnknown        = "Unknown"
)

// hostnameMatch — `kubernetes.io/hostname` label regex → provider. Order
// matters: first match wins, so we use a slice.
var hostnameMatch = []struct {
	provider string
	pattern  *regexp.Regexp
}{
	{providerKind, regexp.MustCompile(`.*kind.*`)},
	{providerRancherDesktop, regexp.MustCompile(`.*rancher-desktop.*`)},
}

// nodeLabelMatch — provider-unique label key. Order mirrors Python's NODE_LABELS
// dict-iteration order (insertion order in 3.7+).
var nodeLabelMatch = []struct {
	provider string
	label    string
}{
	{providerMinikube, "minikube.k8s.io/name"},
	{providerDigitalOcean, "doks.digitalocean.com/version"},
	{providerKops, "kops.k8s.io/instancegroup"},
	{providerKapsule, "k8s.scaleway.com/kapsule"},
	{providerCivo, "kubernetes.civo.com/civo-node-size"},
}

// IMDSv2 endpoints — vars (not consts) so tests can override.
//
// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/instancedata-data-retrieval.html
var (
	imdsTokenURL    = "http://169.254.169.254/latest/api/token"
	imdsDocumentURL = "http://169.254.169.254/latest/dynamic/instance-identity/document"
	imdsTimeout     = 2 * time.Second
)

var azureProviderIDRE = regexp.MustCompile(`(?i)/subscriptions/([^/]+)/resourceGroups/([^/]+)/`)

// DetectProvider does a single Nodes().List(), runs the detection chain,
// parses metadata, and (for AWS) hits IMDSv2. Returns zero-value ProviderInfo
// on any error — caller treats empty fields as unknown.
func DetectProvider(ctx context.Context, cs kubernetes.Interface, logger *slog.Logger) ProviderInfo {
	if logger == nil {
		logger = slog.Default()
	}
	if cs == nil {
		return ProviderInfo{}
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		logger.Warn("cluster provider detection: node list failed", "err", err)
		return ProviderInfo{}
	}
	nodes := list.Items
	if len(nodes) == 0 {
		return ProviderInfo{}
	}
	info := ProviderInfo{Provider: findClusterProvider(nodes)}
	info.Region, info.Zone, info.AccountNumber, info.ProjectID, info.ResourceGroup = parseProviderMetadata(ctx, nodes)
	return info
}

// findClusterProvider runs the 6-step classification chain on the node list.
func findClusterProvider(nodes []corev1.Node) string {
	if p := detectProviderFromHostname(nodes); p != providerUnknown {
		return p
	}
	if isInProviderID(nodes, "aks") {
		return providerAKS
	}
	if isInKubeletVersion(nodes, "gke") {
		return providerGKE
	}
	if isInKubeletVersion(nodes, "eks") {
		return providerEKS
	}
	if isInProviderID(nodes, "kind") {
		return providerKind
	}
	return detectProviderFromNodeLabels(nodes)
}

func detectProviderFromHostname(nodes []corev1.Node) string {
	for _, n := range nodes {
		host, ok := n.Labels["kubernetes.io/hostname"]
		if !ok || host == "" {
			continue
		}
		for _, hm := range hostnameMatch {
			if hm.pattern.MatchString(host) {
				return hm.provider
			}
		}
	}
	return providerUnknown
}

func detectProviderFromNodeLabels(nodes []corev1.Node) string {
	if len(nodes) == 0 {
		return providerUnknown
	}
	for _, lm := range nodeLabelMatch {
		if _, ok := nodes[0].Labels[lm.label]; ok {
			return lm.provider
		}
	}
	return providerUnknown
}

// isInProviderID returns true when nodes[0].Spec.ProviderID contains the substring.
// Mirrors `_is_str_in_cluster_provider`.
func isInProviderID(nodes []corev1.Node, sub string) bool {
	if len(nodes) == 0 {
		return false
	}
	return strings.Contains(nodes[0].Spec.ProviderID, sub)
}

// isInKubeletVersion returns true when nodes[0].Status.NodeInfo.KubeletVersion
// contains the substring. Mirrors `_is_detect_cluster_from_kubelet_version`.
func isInKubeletVersion(nodes []corev1.Node, sub string) bool {
	if len(nodes) == 0 {
		return false
	}
	return strings.Contains(nodes[0].Status.NodeInfo.KubeletVersion, sub)
}

// parseProviderMetadata extracts region, zone, account, project, resource-group
// from nodes[0]. Mirrors `_parse_provider_metadata`. Returns empty strings for
// fields that don't apply to the detected cloud.
func parseProviderMetadata(ctx context.Context, nodes []corev1.Node) (region, zone, account, project, rg string) {
	if len(nodes) == 0 {
		return
	}
	labels := nodes[0].Labels
	region = labels["topology.kubernetes.io/region"]
	zone = labels["topology.kubernetes.io/zone"]

	pid := nodes[0].Spec.ProviderID
	switch {
	case strings.HasPrefix(pid, "aws://"):
		account = imdsAccountID(ctx)
	case strings.HasPrefix(pid, "gce://"):
		// gce://<project>/<zone>/<instance>
		parts := strings.Split(strings.TrimPrefix(pid, "gce://"), "/")
		if len(parts) >= 3 {
			project = parts[0]
			account = parts[0]
		}
	case strings.HasPrefix(pid, "azure://"):
		if m := azureProviderIDRE.FindStringSubmatch(pid); len(m) == 3 {
			account = m[1]
			rg = m[2]
		}
	}
	return
}

// imdsAccountID hits IMDSv2 to read the EC2 instance identity document and
// returns the accountId. Returns "" on any error (Fargate, hop-limit blocked,
// non-AWS, network policy, etc.) — fail-silent.
func imdsAccountID(ctx context.Context) string {
	client := &http.Client{Timeout: imdsTimeout}

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsTokenURL, nil)
	if err != nil {
		return ""
	}
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return ""
	}
	defer func() { _ = tokenResp.Body.Close() }()
	if tokenResp.StatusCode >= 400 {
		return ""
	}
	tokenBytes, err := io.ReadAll(io.LimitReader(tokenResp.Body, 4096))
	if err != nil {
		return ""
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return ""
	}

	docReq, err := http.NewRequestWithContext(ctx, http.MethodGet, imdsDocumentURL, nil)
	if err != nil {
		return ""
	}
	docReq.Header.Set("X-aws-ec2-metadata-token", token)
	docResp, err := client.Do(docReq)
	if err != nil {
		return ""
	}
	defer func() { _ = docResp.Body.Close() }()
	if docResp.StatusCode >= 400 {
		return ""
	}
	var doc struct {
		AccountID string `json:"accountId"`
	}
	if err := json.NewDecoder(io.LimitReader(docResp.Body, 64<<10)).Decode(&doc); err != nil {
		return ""
	}
	return doc.AccountID
}
