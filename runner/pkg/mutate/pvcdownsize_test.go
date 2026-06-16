package mutate

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/nudgebee/nudgebee-agent/pkg/podexec"
)

// fakeExec stubs the SPDY pod-exec. The real mover pod never runs under the
// fake clientset, so the copy is simulated by the configured result.
type fakeExec struct {
	res   *podexec.Result
	err   error
	calls int
}

func (f *fakeExec) Exec(_ context.Context, _ *podexec.Request) (*podexec.Result, error) {
	f.calls++
	return f.res, f.err
}

// markPodsRunning makes every created pod immediately Running so
// waitForPodRunning returns without the real scheduler.
func markPodsRunning(cs *fake.Clientset) {
	cs.PrependReactor("create", "pods", func(a ktesting.Action) (bool, runtime.Object, error) {
		pod := a.(ktesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil // fall through so the tracker stores the mutated pod
	})
}

func q(s string) resource.Quantity { return resource.MustParse(s) }

func boundPVC(ns, name, sc, pvName, size string) *corev1.PersistentVolumeClaim {
	scn := sc
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": "x"}},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &scn,
			VolumeName:       pvName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: q(size)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase:    corev1.ClaimBound,
			Capacity: corev1.ResourceList{corev1.ResourceStorage: q(size)},
		},
	}
}

func pvWith(name string, reclaim corev1.PersistentVolumeReclaimPolicy) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: reclaim},
	}
}

func deploymentMounting(ns, name, pvcName string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Volumes: []corev1.Volume{{Name: "d", VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName},
				}}},
			}},
		},
	}
}

func fastSC() *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fast"}}
}

func newMigrator(cs *fake.Clientset, fe podexec.Executor) *Mutator {
	m := New(cs, "", nil)
	m.SetExec(fe)
	m.opTimeoutOverride = 2 * time.Second
	m.podTermTimeoutOverride = 2 * time.Second
	m.pollIntervalOverride = 2 * time.Millisecond
	return m
}

func listPVCNames(t *testing.T, cs *fake.Clientset, ns string) []string {
	t.Helper()
	l, err := cs.CoreV1().PersistentVolumeClaims(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range l.Items {
		names = append(names, p.Name)
	}
	return names
}

func TestDownsizeDeployment_HappyPath(t *testing.T) {
	cs := fake.NewClientset(
		boundPVC("data", "vol", "fast", "pv-1", "20Gi"),
		pvWith("pv-1", corev1.PersistentVolumeReclaimDelete),
		fastSC(),
		deploymentMounting("data", "web", "vol", 2),
	)
	markPodsRunning(cs)
	m := newMigrator(cs, &fakeExec{res: &podexec.Result{Stdout: "COPY_SUCCESS", ExitCode: 0}})

	got, err := m.DownsizePVC(context.Background(), "data", "vol", q("10Gi"))
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["success"] != true {
		t.Fatalf("success=%v", got)
	}

	// Original PVC deleted; a downsized PVC exists at the new size.
	var newName string
	for _, n := range listPVCNames(t, cs, "data") {
		if n == "vol" {
			t.Error("original PVC should be deleted")
		}
		if strings.HasPrefix(n, "vol-downsized-") {
			newName = n
		}
	}
	if newName == "" {
		t.Fatal("downsized PVC not found")
	}
	newPVC, _ := cs.CoreV1().PersistentVolumeClaims("data").Get(context.Background(), newName, metav1.GetOptions{})
	if got := newPVC.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(q("10Gi")) != 0 {
		t.Errorf("new pvc size = %s; want 10Gi", got.String())
	}
	// Deployment repointed and scaled back up.
	dep, _ := cs.AppsV1().Deployments("data").Get(context.Background(), "web", metav1.GetOptions{})
	if cn := dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; cn != newName {
		t.Errorf("deployment claim = %q; want %q", cn, newName)
	}
	if *dep.Spec.Replicas != 2 {
		t.Errorf("replicas = %d; want 2 restored", *dep.Spec.Replicas)
	}
}

func TestDownsizeStatefulSet_HappyPath(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr32(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "db"}},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{ObjectMeta: metav1.ObjectMeta{Name: "data"}},
			},
		},
	}
	cs := fake.NewClientset(
		boundPVC("data", "data-db-0", "fast", "pv-2", "20Gi"),
		pvWith("pv-2", corev1.PersistentVolumeReclaimDelete),
		fastSC(),
		sts,
	)
	markPodsRunning(cs)
	fe := &fakeExec{res: &podexec.Result{Stdout: "COPY_SUCCESS", ExitCode: 0}}
	m := newMigrator(cs, fe)

	if _, err := m.DownsizePVC(context.Background(), "data", "data-db-0", q("10Gi")); err != nil {
		t.Fatal(err)
	}
	if fe.calls != 2 {
		t.Errorf("expected 2 copies (orig→temp, temp→orig), got %d", fe.calls)
	}
	// Original name preserved at new size; temp PVC cleaned up.
	final, err := cs.CoreV1().PersistentVolumeClaims("data").Get(context.Background(), "data-db-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("original-name PVC must survive: %v", err)
	}
	if got := final.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(q("10Gi")) != 0 {
		t.Errorf("final size = %s; want 10Gi", got.String())
	}
	for _, n := range listPVCNames(t, cs, "data") {
		if strings.Contains(n, "-tmp-") {
			t.Errorf("temp PVC %q should be cleaned up", n)
		}
	}
	// Orphaned PV deleted on success.
	if _, err := cs.CoreV1().PersistentVolumes().Get(context.Background(), "pv-2", metav1.GetOptions{}); err == nil {
		t.Error("orphaned PV should be deleted")
	}
	sf, _ := cs.AppsV1().StatefulSets("data").Get(context.Background(), "db", metav1.GetOptions{})
	if *sf.Spec.Replicas != 1 {
		t.Errorf("replicas = %d; want 1 restored", *sf.Spec.Replicas)
	}
}

func TestDownsizeDeployment_RollbackOnCopyFailure(t *testing.T) {
	cs := fake.NewClientset(
		boundPVC("data", "vol", "fast", "pv-1", "20Gi"),
		pvWith("pv-1", corev1.PersistentVolumeReclaimDelete),
		fastSC(),
		deploymentMounting("data", "web", "vol", 3),
	)
	markPodsRunning(cs)
	// Copy fails before the point of no return.
	m := newMigrator(cs, &fakeExec{res: &podexec.Result{Stdout: "", ExitCode: 1, Stderr: "disk full"}})

	if _, err := m.DownsizePVC(context.Background(), "data", "vol", q("10Gi")); err == nil {
		t.Fatal("expected copy failure to abort")
	}
	// Original intact, new PVC cleaned up, replicas restored, claim unchanged.
	if _, err := cs.CoreV1().PersistentVolumeClaims("data").Get(context.Background(), "vol", metav1.GetOptions{}); err != nil {
		t.Error("original PVC must remain after rollback")
	}
	for _, n := range listPVCNames(t, cs, "data") {
		if strings.HasPrefix(n, "vol-downsized-") {
			t.Errorf("new PVC %q should be deleted on rollback", n)
		}
	}
	dep, _ := cs.AppsV1().Deployments("data").Get(context.Background(), "web", metav1.GetOptions{})
	if *dep.Spec.Replicas != 3 {
		t.Errorf("replicas = %d; want 3 restored", *dep.Spec.Replicas)
	}
	if cn := dep.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName; cn != "vol" {
		t.Errorf("claim = %q; should be unchanged 'vol'", cn)
	}
}

func TestDownsize_RequiresExec(t *testing.T) {
	cs := fake.NewClientset(boundPVC("data", "vol", "fast", "pv-1", "20Gi"), pvWith("pv-1", "Delete"), fastSC())
	m := New(cs, "", nil) // no SetExec
	if _, err := m.DownsizePVC(context.Background(), "data", "vol", q("10Gi")); err == nil {
		t.Fatal("expected error when exec capability is absent")
	}
}

func TestValidateDownsizePrereqs(t *testing.T) {
	// not Bound
	pvc := boundPVC("data", "vol", "fast", "pv-1", "20Gi")
	pvc.Status.Phase = corev1.ClaimPending
	cs := fake.NewClientset(pvc, pvWith("pv-1", "Delete"), fastSC())
	m := newMigrator(cs, &fakeExec{res: &podexec.Result{}})
	if _, _, err := m.validateDownsizePrereqs(context.Background(), "data", "vol"); err == nil {
		t.Error("expected error for non-Bound PVC")
	}

	// hostPath PV
	pv := pvWith("pv-h", "Delete")
	pv.Spec.HostPath = &corev1.HostPathVolumeSource{Path: "/tmp"}
	cs2 := fake.NewClientset(boundPVC("data", "vh", "fast", "pv-h", "20Gi"), pv, fastSC())
	m2 := newMigrator(cs2, &fakeExec{res: &podexec.Result{}})
	if _, _, err := m2.validateDownsizePrereqs(context.Background(), "data", "vh"); err == nil {
		t.Error("expected error for hostPath PV")
	}
}

func TestGetWorkloadByPVC_StatefulSetTemplate(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "data"},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{Name: "data"}}},
		},
	}
	cs := fake.NewClientset(sts)
	m := New(cs, "", nil)
	_, kind, err := m.getWorkloadByPVC(context.Background(), "data", "data-db-0")
	if err != nil || kind != "StatefulSet" {
		t.Fatalf("kind=%q err=%v; want StatefulSet", kind, err)
	}
	// non-matching ordinal suffix
	_, kind, _ = m.getWorkloadByPVC(context.Background(), "data", "data-db-x")
	if kind != "" {
		t.Errorf("kind=%q; want no match for non-numeric ordinal", kind)
	}
}

func TestVerifyParity(t *testing.T) {
	src := boundPVC("data", "vol", "fast", "pv-1", "20Gi")
	snap := newPVCSnapshot(src)
	// faithful clone at 10Gi passes
	clone := boundPVC("data", "vol2", "fast", "", "10Gi")
	if mm := snap.verifyParity(clone, q("10Gi")); len(mm) != 0 {
		t.Errorf("expected parity, got %v", mm)
	}
	// wrong storageclass fails
	bad := boundPVC("data", "vol3", "slow", "", "10Gi")
	if mm := snap.verifyParity(bad, q("10Gi")); len(mm) == 0 {
		t.Error("expected storageClass mismatch")
	}
}

func ptr32(i int32) *int32 { return &i }

// TestRepointDeploymentPVC_Idempotent covers the retry case where a prior
// Update applied server-side but the client saw a conflict: the re-Get shows
// the volume already on newPVC, which must be treated as success (not a hard
// "no volume referencing oldPVC" error that would fail the migration).
func TestRepointDeploymentPVC_Idempotent(t *testing.T) {
	cs := fake.NewClientset(deploymentMounting("data", "web", "data-new", 2))
	m := New(cs, "", nil)
	if err := m.repointDeploymentPVC(context.Background(), "data", "web", "data-old", "data-new"); err != nil {
		t.Fatalf("already-repointed deployment should succeed, got %v", err)
	}
	// genuinely-missing volume still errors.
	if err := m.repointDeploymentPVC(context.Background(), "data", "web", "data-old", "data-other"); err == nil {
		t.Error("expected error when no volume references oldPVC and not already repointed")
	}
}
