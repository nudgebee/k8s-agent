// Package podrunner implements pod_script_run_enricher with dedicated-pod
// semantics — the wire shape api-server's relay.CommandExecutor and the
// runbook/LLM/relay-server callers have used since the legacy agent.
//
// Contract (params):
//
//	image                (string, required)  container image to spawn
//	command              (string, required)  shell script body (raw text)
//	pod_name             (string, optional)  pod name; generated if empty
//	namespace            (string, optional)  defaults to runner DefaultNamespace
//	secret               (string, optional)  k8s Secret name; sourced via envFrom
//	                                         "ns/name" splits into namespace + secret
//	env_variables        (map,    optional)  plain key/value env vars
//	env_from_secret_keys (map,    optional)  informational; ignored at runtime
//	                                         (envFrom already pulls all keys)
//	wait_threshold       (number, optional)  pod-completion timeout in minutes (default 1)
//
// Flow:
//  1. Base64-encode `command` for transport into the pod via ConfigMap.
//  2. Create a ConfigMap holding the encoded script.
//  3. Spawn a Pod (restartPolicy: Never) with the configured image, configmap
//     mounted at /mnt, envFrom: secret (if any), env: env_variables. Container
//     command is `sh -c 'base64 -d /mnt/script > /tmp/script.sh && sh /tmp/script.sh'`.
//  4. Poll pod status; fail fast on unrecoverable container errors
//     (ImagePullBackOff, ErrImagePull, CreateContainerConfigError, …).
//  5. On Succeeded/Failed: read logs.
//  6. Always clean up pod + configmap.
//
// Response shape (consumed by api-server relay.ExecuteAndExtractResponse →
// CommandExecutor):
//
//	{
//	  "success": true,
//	  "findings": [{
//	    "evidence": [{
//	      "data": "[{\"type\":\"json\",\"data\":\"<json-of response_dict>\",\"additional_info\":null}]"
//	    }]
//	  }]
//	}
//
// where response_dict carries the input params + `type` + `response` (stdout
// of the pod). The single JsonBlock-in-Finding shape is what callers parse.
package podrunner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/nudgebee/nudgebee-agent/pkg/enrichers"
)

// Runner spawns dedicated pods to execute scripts, sourcing env vars from
// k8s Secrets. One Runner per agent process; safe for concurrent use.
type Runner struct {
	Client           kubernetes.Interface
	DefaultNamespace string        // used when params.namespace is empty
	ServiceAccount   string        // pod.spec.serviceAccountName; empty → cluster default
	AccountID        string        // tenant id; passed through to FindingResponse
	PollInterval     time.Duration // status poll cadence; default 5s
	Now              func() time.Time
}

// New builds a Runner with defaults filled in.
func New(cs kubernetes.Interface, defaultNS, sa, accountID string) *Runner {
	return &Runner{
		Client:           cs,
		DefaultNamespace: defaultNS,
		ServiceAccount:   sa,
		AccountID:        accountID,
		PollInterval:     5 * time.Second,
		Now:              time.Now,
	}
}

// Common waiting-state reasons that won't resolve on their own.
var failFastReasons = map[string]struct{}{
	"ImagePullBackOff":           {},
	"ErrImagePull":               {},
	"CreateContainerConfigError": {},
	"InvalidImageName":           {},
	"CreateContainerError":       {},
	"RunContainerError":          {},
}

// Handle executes one pod_script_run_enricher invocation. Returns the
// FindingResponse-shaped map on success; on failure it still returns a
// FindingResponse but with the error message in `response_dict.response`
// — callers always expect a JsonBlock back, success or fail.
func (r *Runner) Handle(ctx context.Context, params map[string]any) (any, error) {
	if r.Client == nil {
		return enrichers.ErrorResponse("podrunner: kubernetes client not configured", 500), nil
	}

	image, _ := params["image"].(string)
	command, _ := params["command"].(string)
	podName, _ := params["pod_name"].(string)
	namespace, _ := params["namespace"].(string)
	secret, _ := params["secret"].(string)

	if image == "" {
		return enrichers.ErrorResponse("pod_script_run_enricher: image is required", 400), nil
	}
	if command == "" {
		return enrichers.ErrorResponse("pod_script_run_enricher: command is required", 400), nil
	}

	// "ns/secret" pattern: api-server's CommandExecutor splits the secret
	// at the first slash. Mirror that here so a caller that didn't pre-split
	// still works.
	if strings.Contains(secret, "/") {
		parts := strings.SplitN(secret, "/", 2)
		if namespace == "" {
			namespace = parts[0]
		}
		secret = parts[1]
	}

	if namespace == "" {
		namespace = r.DefaultNamespace
	}
	if podName == "" {
		podName = "nb-pod-" + uuid.NewString()
	}

	waitMinutes := 1
	if v, ok := params["wait_threshold"].(float64); ok && v >= 1 {
		waitMinutes = int(v)
	}
	if v, ok := params["wait_threshold"].(int); ok && v >= 1 {
		waitMinutes = v
	}

	envVars := decodeStringMap(params["env_variables"])

	// `command` is always raw shell text — we DON'T try to auto-detect
	// base64. The heuristic (try decode, fall back on error) silently
	// corrupts any command that happens to be valid base64: 4-char
	// commands like `echo`, `date`, `test` decode to 3 bytes of binary
	// and the pod runs garbage. If a future caller needs to ship binary
	// payloads, add an explicit `command_b64` param rather than guessing.
	encodedForCM := base64.StdEncoding.EncodeToString([]byte(command))

	cmName := fmt.Sprintf("nb-script-%s", shortUUID())
	if err := r.createConfigMap(ctx, namespace, cmName, encodedForCM); err != nil {
		return r.failure(params, namespace, podName, image, command, secret,
			fmt.Sprintf("create configmap: %v", err)), nil
	}
	defer r.deleteConfigMap(namespace, cmName)

	if err := r.createPod(ctx, namespace, podName, image, cmName, secret, envVars); err != nil {
		return r.failure(params, namespace, podName, image, command, secret,
			fmt.Sprintf("create pod: %v", err)), nil
	}
	defer r.deletePod(namespace, podName)

	logs, waitErr := r.waitAndReadLogs(ctx, namespace, podName, time.Duration(waitMinutes)*time.Minute)
	if waitErr != nil {
		return r.failure(params, namespace, podName, image, command, secret,
			waitErr.Error()), nil
	}

	return r.success(params, namespace, podName, image, command, secret, logs), nil
}

func (r *Runner) createConfigMap(ctx context.Context, ns, name, b64Script string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Data:       map[string]string{"script": b64Script},
	}
	_, err := r.Client.CoreV1().ConfigMaps(ns).Create(ctx, cm, metav1.CreateOptions{})
	return err
}

func (r *Runner) deleteConfigMap(ns, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.Client.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("podrunner: configmap cleanup failed", "namespace", ns, "name", name, "err", err)
	}
}

func (r *Runner) createPod(ctx context.Context, ns, name, image, cmName, secret string, env map[string]string) error {
	volume := "script-volume"
	envFrom := []corev1.EnvFromSource{}
	if secret != "" {
		envFrom = append(envFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			},
		})
	}
	envVars := make([]corev1.EnvVar, 0, len(env))
	for k, v := range env {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			ServiceAccountName: r.ServiceAccount,
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:            "runner-container",
					Image:           image,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command: []string{"sh", "-c",
						"base64 -d /mnt/script > /tmp/script.sh && sh /tmp/script.sh"},
					EnvFrom: envFrom,
					Env:     envVars,
					VolumeMounts: []corev1.VolumeMount{
						{Name: volume, MountPath: "/mnt"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: volume,
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
						},
					},
				},
			},
		},
	}
	_, err := r.Client.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	return err
}

func (r *Runner) deletePod(ns, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := r.Client.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("podrunner: pod cleanup failed", "namespace", ns, "name", name, "err", err)
	}
}

// waitAndReadLogs polls pod status until the pod completes or the timeout
// expires. Returns the pod log content as a string.
func (r *Runner) waitAndReadLogs(ctx context.Context, ns, name string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = time.Minute
	}
	poll := r.PollInterval
	if poll <= 0 {
		poll = 5 * time.Second
	}
	deadline := r.Now().Add(timeout)

	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		pod, err := r.Client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return "", fmt.Errorf("pod %s/%s disappeared during wait", ns, name)
			}
			// Transient — retry on next tick.
			slog.Warn("podrunner: pod get failed, retrying", "namespace", ns, "name", name, "err", err)
		} else {
			if reason := detectUnrecoverable(pod); reason != "" {
				return "", errors.New(reason)
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				return r.readPodLogs(ctx, ns, name)
			}
		}

		if r.Now().After(deadline) {
			return "", fmt.Errorf("pod %s/%s did not complete within %s", ns, name, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (r *Runner) readPodLogs(ctx context.Context, ns, name string) (string, error) {
	req := r.Client.CoreV1().Pods(ns).GetLogs(name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("open log stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var sb strings.Builder
	buf := make([]byte, 8192)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if err != nil {
			// io.EOF is expected end-of-stream.
			break
		}
	}
	return sb.String(), nil
}

// detectUnrecoverable scans (init + main) container statuses for waiting
// reasons we know won't recover, plus any state.terminated.reason that
// indicates a config-level failure.
func detectUnrecoverable(pod *corev1.Pod) string {
	for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...), pod.Status.InitContainerStatuses...) {
		if cs.State.Waiting != nil {
			if _, bad := failFastReasons[cs.State.Waiting.Reason]; bad {
				return fmt.Sprintf("container %s waiting: %s — %s", cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
		}
	}
	return ""
}

func (r *Runner) success(params map[string]any, ns, pod, image, command, secret, logs string) map[string]any {
	return r.respond(params, ns, pod, image, command, secret, logs, "")
}

func (r *Runner) failure(params map[string]any, ns, pod, image, command, secret, errMsg string) map[string]any {
	return r.respond(params, ns, pod, image, command, secret, "", errMsg)
}

// respond builds the Finding-shaped response wrapping a single JsonBlock
// whose data is the JSON-encoded responseDict. api-server's
// relay.CommandExecutor parses out response_dict["response"] as the stdout.
func (r *Runner) respond(params map[string]any, ns, pod, image, command, secret, logs, errMsg string) map[string]any {
	responseDict := map[string]any{
		"type":      "pod_script_run_enricher",
		"namespace": ns,
		"pod_name":  pod,
		"image":     image,
		"command":   command,
		"secret":    secret,
		"response":  logs,
	}
	// Pass through fields api-server callers also expect to see echoed.
	if v, ok := params["env_from_secret_keys"]; ok {
		responseDict["env_from_secret_keys"] = v
	}
	if v, ok := params["env_variables"]; ok {
		responseDict["env_variables"] = v
	}
	if v, ok := params["wait_threshold"]; ok {
		responseDict["wait_threshold"] = v
	}
	if errMsg != "" {
		// Surface error text in the `response` field on failure — callers
		// only read `response`, so packing the error there keeps a single
		// extraction path for the success and failure cases.
		responseDict["response"] = errMsg
		responseDict["error"] = errMsg
	}

	block, err := enrichers.JSONBlock(responseDict)
	if err != nil {
		return enrichers.ErrorResponse(fmt.Sprintf("podrunner: marshal response: %v", err), 500)
	}
	resp, err := enrichers.FindingResponse(r.AccountID, uuid.Nil, block)
	if err != nil {
		return enrichers.ErrorResponse(fmt.Sprintf("podrunner: build finding: %v", err), 500)
	}
	return resp
}

func decodeStringMap(v any) map[string]string {
	out := map[string]string{}
	if v == nil {
		return out
	}
	if m, ok := v.(map[string]string); ok {
		for k, val := range m {
			out[k] = val
		}
		return out
	}
	if m, ok := v.(map[string]any); ok {
		for k, val := range m {
			if s, ok := val.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func shortUUID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
}
