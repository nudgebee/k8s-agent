//go:build live

// Live integration tests for the agent_task remediation actions, run against a
// REAL cluster (kubeconfig). They are excluded from the normal build by the
// `live` tag — the fake-client unit tests cover the logic; these prove the
// actions work end-to-end where pods actually run and PVCs bind to real disks
// (which envtest can't do).
//
// Run (defaults suit a pd-balanced cluster):
//
//	go test -tags live ./pkg/mutate -run TestLive -v -timeout 25m
//
// On a C4/hyperdisk node pool, pd-balanced won't attach — use the hyperdisk
// class and its >=4Gi sizes:
//
//	LIVE_SC=hyperdisk-balanced-rwo LIVE_BASE_SIZE=6Gi LIVE_EXPAND_SIZE=8Gi LIVE_DOWNSIZE_SIZE=4Gi \
//	  go test -tags live ./pkg/mutate -run TestLive -v -timeout 25m
//
// Each test creates a throwaway namespace and deletes it on cleanup.
package mutate

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nudgebee/nudgebee-agent/internal/k8sclient"
	"github.com/nudgebee/nudgebee-agent/pkg/podexec"
)

func liveSC() string { return envOr("LIVE_SC", "standard-rwo") }

// Sizes are env-tunable because the floor depends on the disk type: pd-balanced
// allows 1Gi, hyperdisk-balanced requires >=4Gi. base > downsize so the
// downsize test actually shrinks. On a C4 node pool, run with:
//
//	LIVE_SC=hyperdisk-balanced-rwo LIVE_BASE_SIZE=6Gi LIVE_EXPAND_SIZE=8Gi LIVE_DOWNSIZE_SIZE=4Gi
func baseSize() string     { return envOr("LIVE_BASE_SIZE", "2Gi") }
func expandSize() string   { return envOr("LIVE_EXPAND_SIZE", "3Gi") }
func downsizeSize() string { return envOr("LIVE_DOWNSIZE_SIZE", "1Gi") }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// liveSetup builds real clients and a fully-wired Mutator, skipping the test if
// no cluster is reachable.
func liveSetup(t *testing.T) (kubernetes.Interface, *rest.Config, *Mutator) {
	t.Helper()
	cs, restCfg, err := k8sclient.New("")
	if err != nil {
		t.Skipf("no cluster: %v", err)
	}
	if _, err := cs.CoreV1().Namespaces().List(context.Background(), metav1.ListOptions{Limit: 1}); err != nil {
		t.Skipf("cluster unreachable: %v", err)
	}
	dyn, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		t.Fatal(err)
	}
	m := New(cs, "", nil)
	m.SetDynamic(dyn)
	m.SetExec(podexec.New(cs, restCfg))
	return cs, restCfg, m
}

func newNamespace(t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	ns := fmt.Sprintf("rs-live-%d", time.Now().UnixNano()%1_000_000)
	if _, err := cs.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
		t.Logf("namespace %s deleted", ns)
	})
	return ns
}

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := fn()
		if err != nil {
			t.Fatalf("waiting for %s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(3 * time.Second)
	}
}

// makePVCAndDeploy creates a PVC and a busybox Deployment that mounts it at
// /data and just sleeps (it does NOT write the volume — data is written by the
// test via exec so we can prove the migration preserved it). Waits until the
// pod is Running and the PVC is Bound.
func makePVCAndDeploy(t *testing.T, cs kubernetes.Interface, ns, pvcName, deploy, size string) {
	t.Helper()
	sc := liveSC()
	if _, err := cs.CoreV1().PersistentVolumeClaims(ns).Create(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: pvcName},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &sc,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pvc: %v", err)
	}
	reps := int32(1)
	if _, err := cs.AppsV1().Deployments(ns).Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: deploy},
		Spec: appsv1.DeploymentSpec{
			Replicas: &reps,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": deploy}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": deploy}},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr64(1),
					Containers: []corev1.Container{{
						Name:         "app",
						Image:        "busybox",
						Command:      []string{"sh", "-c", "sleep 100000"},
						VolumeMounts: []corev1.VolumeMount{{Name: "v", MountPath: "/data"}},
					}},
					Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
					}}},
				},
			},
		},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create deploy: %v", err)
	}
	waitFor(t, "pod Running", 4*time.Minute, func() (bool, error) {
		return podRunning(cs, ns, "app="+deploy)
	})
	waitFor(t, "pvc Bound", 2*time.Minute, func() (bool, error) {
		p, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), pvcName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return p.Status.Phase == corev1.ClaimBound, nil
	})
}

func podRunning(cs kubernetes.Interface, ns, selector string) (bool, error) {
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return true, nil
		}
	}
	return false, nil
}

func podName(t *testing.T, cs kubernetes.Interface, ns, selector string) string {
	t.Helper()
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			return p.Name
		}
	}
	t.Fatalf("no running pod for %s", selector)
	return ""
}

func execSh(t *testing.T, restCfg *rest.Config, cs kubernetes.Interface, ns, pod, cmd string) string {
	t.Helper()
	res, err := podexec.New(cs, restCfg).Exec(context.Background(), &podexec.Request{
		Namespace: ns, Pod: pod, Command: []string{"sh", "-c", cmd}, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("exec %q: %v", cmd, err)
	}
	return strings.TrimSpace(res.Stdout)
}

func ptr64(i int64) *int64 { return &i }

// --- tests ---

func TestLiveExpandPVC(t *testing.T) {
	cs, _, m := liveSetup(t)
	ns := newNamespace(t, cs)
	makePVCAndDeploy(t, cs, ns, "data", "web", baseSize())

	if _, err := m.RightsizePVC(context.Background(), ns, "data", expandSize()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pvc request 3Gi", time.Minute, func() (bool, error) {
		p, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), "data", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		q := p.Spec.Resources.Requests[corev1.ResourceStorage]
		return q.Cmp(resource.MustParse(expandSize())) == 0, nil
	})
}

func TestLiveReplicaScale(t *testing.T) {
	cs, _, m := liveSetup(t)
	ns := newNamespace(t, cs)
	makePVCAndDeploy(t, cs, ns, "data", "web", baseSize())

	if _, err := m.ScaleWorkload(context.Background(), "Deployment", ns, "web", 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "scaled to 0", time.Minute, func() (bool, error) {
		d, err := cs.AppsV1().Deployments(ns).Get(context.Background(), "web", metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		return d.Status.Replicas == 0, nil
	})
	if _, err := m.ScaleWorkload(context.Background(), "Deployment", ns, "web", 1); err != nil {
		t.Fatal(err)
	}
}

func TestLiveDownsizeDeployment(t *testing.T) {
	cs, restCfg, m := liveSetup(t)
	ns := newNamespace(t, cs)
	makePVCAndDeploy(t, cs, ns, "data", "web", baseSize())

	// Write a token the container never touches, so it survives only if the
	// migration copied the data.
	const token = "nudgebee-downsize-ok"
	pod := podName(t, cs, ns, "app=web")
	execSh(t, restCfg, cs, ns, pod, "echo "+token+" > /data/marker.txt && sync")

	// Downsize 2Gi -> 1Gi (GKE PD minimum).
	if _, err := m.RightsizePVC(context.Background(), ns, "data", downsizeSize()); err != nil {
		t.Fatalf("downsize: %v", err)
	}

	// Original PVC gone; a downsized PVC exists at 1Gi.
	pvcs, _ := cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	var newName string
	for _, p := range pvcs.Items {
		if p.Name == "data" {
			t.Error("original PVC should be deleted")
		}
		if strings.HasPrefix(p.Name, "data-downsized-") {
			newName = p.Name
			if q := p.Spec.Resources.Requests[corev1.ResourceStorage]; q.Cmp(resource.MustParse(downsizeSize())) != 0 {
				t.Errorf("new pvc size = %s; want %s", q.String(), downsizeSize())
			}
		}
	}
	if newName == "" {
		t.Fatal("downsized PVC not found")
	}

	// Workload back up on the new PVC, data intact.
	waitFor(t, "pod Running on new PVC", 4*time.Minute, func() (bool, error) {
		return podRunning(cs, ns, "app=web")
	})
	got := execSh(t, restCfg, cs, ns, podName(t, cs, ns, "app=web"), "cat /data/marker.txt")
	if got != token {
		t.Fatalf("data not preserved: marker=%q want %q", got, token)
	}
	t.Logf("downsize OK: data survived on %s", newName)
}

func TestLiveVolumeDelete(t *testing.T) {
	cs, _, m := liveSetup(t)
	ns := newNamespace(t, cs)
	makePVCAndDeploy(t, cs, ns, "data", "web", baseSize())

	// Resolve the bound PV, then release the consumer so the PVC delete won't hang.
	pvc, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), "data", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pvName := pvc.Spec.VolumeName
	if err := cs.AppsV1().Deployments(ns).Delete(context.Background(), "web", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "consumer pod gone", 2*time.Minute, func() (bool, error) {
		pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=web"})
		if err != nil {
			return false, err
		}
		return len(pods.Items) == 0, nil
	})

	if _, err := m.DeleteVolume(context.Background(), pvName); err != nil {
		t.Fatalf("volume_delete: %v", err)
	}
	// Bound PVC gone.
	waitFor(t, "pvc deleted", 2*time.Minute, func() (bool, error) {
		_, err := cs.CoreV1().PersistentVolumeClaims(ns).Get(context.Background(), "data", metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
	// PV gone (explicit delete, or auto-removed for reclaim=Delete).
	waitFor(t, "pv deleted", 2*time.Minute, func() (bool, error) {
		_, err := cs.CoreV1().PersistentVolumes().Get(context.Background(), pvName, metav1.GetOptions{})
		return apierrors.IsNotFound(err), nil
	})
}
