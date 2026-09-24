package enrichers

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// LogsEnricher implements the `logs_enricher` action by fetching pod
// logs through the K8s API and wrapping them in a FileBlock-shaped Finding.
//
// The api-server caller (eventrule_actions_logs.go) sends:
//
//	{"name": "<workload-or-pod-name>", "namespace": "<ns>"}
//
// and parses the response with relay.ExecuteAndExtractResponse, then reads
// `data` (string), `filename` (string), `type` (string) from the resulting
// block. Our FileBlock helper produces all three.
type LogsEnricher struct {
	clientset kubernetes.Interface
	accountID string
}

// NewLogsEnricher wires the action to a typed K8s client. accountID is
// stamped into the returned Finding's evidence.
func NewLogsEnricher(cs kubernetes.Interface, accountID string) *LogsEnricher {
	return &LogsEnricher{clientset: cs, accountID: accountID}
}

// Handle implements `logs_enricher`. Resolves `name` to a pod (either as a pod
// name directly, or as a workload whose pods we list and pick the first one),
// reads logs, returns a FileBlock-shaped Finding.
//
// Optional params (LogEnricherParams):
//
//	container_name        string
//	previous              bool
//	tail_lines            int (default 1000)
//	since_time            int (unix seconds; read no logs older than this)
//
// Lines are always returned with the kubelet's RFC3339Nano timestamp prefixed —
// see readLogs.
func (l *LogsEnricher) Handle(ctx context.Context, params map[string]any) (any, error) {
	if l.clientset == nil {
		return ErrorResponse("logs_enricher: kube client unavailable", 503), nil
	}
	name, _ := params["name"].(string)
	namespace, _ := params["namespace"].(string)
	if name == "" || namespace == "" {
		return ErrorResponse("logs_enricher: name and namespace required", 400), nil
	}
	containerName, _ := params["container_name"].(string)
	previous, _ := params["previous"].(bool)
	tailLines := 1000
	if v, err := toInt(params["tail_lines"]); err == nil && v > 0 {
		tailLines = v
	}
	// since_time has been in this action's contract since it was written and was
	// never read, so a caller asking for the lead-up to an incident silently got
	// the same tail as everyone else.
	var sinceTime *metav1.Time
	if v, err := toInt(params["since_time"]); err == nil && v > 0 {
		t := metav1.NewTime(time.Unix(int64(v), 0).UTC())
		sinceTime = &t
	}

	pod, err := l.resolvePod(ctx, namespace, name)
	if err != nil {
		return ErrorResponse(fmt.Sprintf("logs_enricher: %v", err), 404), nil
	}
	if containerName == "" {
		containerName = pickContainer(pod)
	}

	logs, err := l.readLogs(ctx, pod, containerName, previous, int64(tailLines), sinceTime)
	if err != nil {
		return ErrorResponse(fmt.Sprintf("logs_enricher: read logs: %v", err), 502), nil
	}

	// Gzip text files (.txt/.log) before sending. The UI's
	// KubernetesPodLogs.tsx:68-71 expects `type === "gz"` and base64-decodes
	// + gunzips. Filename gets `.gz` suffix; type derives from the new
	// extension, so we must change the filename, not just compress.
	gzLogs, err := gzipBytes(logs)
	if err != nil {
		return ErrorResponse(fmt.Sprintf("logs_enricher: gzip logs: %v", err), 500), nil
	}
	filename := pod.Name + ".log.gz"
	block := FileBlock(filename, gzLogs)
	// Stamp additional_info with pod/container/namespace for downstream
	// insight extraction.
	block["additional_info"] = map[string]any{
		"pod_name":       pod.Name,
		"container_name": containerName,
		"namespace":      pod.Namespace,
	}
	return FindingResponse(l.accountID, uuid.Nil, block)
}

// gzipBytes compresses with the default gzip level (matches Python's
// gzip.compress default).
func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(b); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resolvePod tries `name` as a pod first; on NotFound it falls back to listing
// pods in `namespace` whose ownerReferences chain (or naming convention) ties
// them to a workload named `name`. Returns the first pod found, preferring
// Running over other phases.
func (l *LogsEnricher) resolvePod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	if pod, err := l.clientset.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{}); err == nil {
		return pod, nil
	}
	// Workload fallback: list and match by owner chain (top-level workload).
	list, err := l.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}
	var fallback *corev1.Pod
	for i := range list.Items {
		p := &list.Items[i]
		if !ownedBy(p, name) {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			return p, nil
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, errors.New("no pods found for workload")
}

// ownedBy returns true if any link in pod's owner chain (walking up via name
// prefix matching, since fake clients don't track ReplicaSet→Deployment) ends
// at a controller named `workload`. We accept both:
//   - pod owned directly by workload (DaemonSet, StatefulSet, …)
//   - pod owned by a ReplicaSet whose name starts with workload+"-" (Deployment).
func ownedBy(pod *corev1.Pod, workload string) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.Name == workload {
			return true
		}
		// Deployment → ReplicaSet → Pod: RS is named "<deploy>-<hash>"
		if ref.Kind == "ReplicaSet" && strings.HasPrefix(ref.Name, workload+"-") {
			return true
		}
	}
	return false
}

// pickContainer returns the container name to read logs from. The legacy
// implementation picks either the alert's container label (if it matches
// one in the pod) or the first container; we don't have alert context
// here, so first container wins.
func pickContainer(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

func (l *LogsEnricher) readLogs(ctx context.Context, pod *corev1.Pod, container string, previous bool, tailLines int64, sinceTime *metav1.Time) ([]byte, error) {
	req := l.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, podLogOptions(container, previous, tailLines, sinceTime))
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, stream); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// podLogOptions is the read this action asks the kubelet for. Split out because
// the fake clientset discards PodLogOptions, so this is the only place the
// contract can be asserted in a test.
func podLogOptions(container string, previous bool, tailLines int64, sinceTime *metav1.Time) *corev1.PodLogOptions {
	return &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tailLines,
		SinceTime: sinceTime,
		// Prefix each line with the kubelet's RFC3339Nano timestamp.
		//
		// Without it the payload says nothing about when it was written, and a tail
		// of a container that has been quiet for weeks is indistinguishable from the
		// output of the crash being investigated: measured against stored evidence
		// on 2026-09-23, 58% of pod-log evidences carried no parseable time on any
		// line, and among those that did, the oldest was 68 days older than the
		// event it was attached to.
		//
		// The api-server strips this prefix before parsing a line as JSON/logfmt
		// (playbooks.splitContainerLogTimestamp), so the grouping, level detection
		// and log card are unaffected; it must ship before this flag reaches a
		// cluster, or structured lines stop being recognised as structured.
		Timestamps: true,
	}
}
