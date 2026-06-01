package discovery

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
)

func TestConvertDeployment(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "frontend", Namespace: "shop", ResourceVersion: "10",
			Labels: map[string]string{"app": "frontend", "app.kubernetes.io/managed-by": "Helm"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(3)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx:1.27"}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 2},
	}
	got, ok := convertDeployment(d)
	if !ok {
		t.Fatal("convertDeployment returned ok=false")
	}
	m := got.(map[string]any)
	// Wire shape: `type` (not service_type) and `service_key`
	// (`<ns>/<type>/<name>`) are the keys the collector reads at
	//
	if m["type"] != "Deployment" {
		t.Errorf("type = %v", m["type"])
	}
	if m["service_key"] != "shop/Deployment/frontend" {
		t.Errorf("service_key = %v", m["service_key"])
	}
	if _, ok := m["config"].(map[string]any); !ok {
		t.Errorf("config (renamed from service_config) = %T", m["config"])
	}
	if m["total_pods"] != int32(3) {
		t.Errorf("total_pods = %v", m["total_pods"])
	}
	if m["ready_pods"] != int32(2) {
		t.Errorf("ready_pods = %v", m["ready_pods"])
	}
	if m["is_helm_release"] != true {
		t.Errorf("is_helm_release should be true via app.kubernetes.io/managed-by")
	}
	// `update_time` is filled in here so the collector can compare against
	// its DB row.
	if _, ok := m["update_time"].(int64); !ok {
		t.Errorf("update_time should be int64 millis; got %T", m["update_time"])
	}
}

func TestConvertStatefulSet(t *testing.T) {
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(2))},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	got, _ := convertStatefulSet(s)
	m := got.(map[string]any)
	if m["type"] != "StatefulSet" || m["total_pods"] != int32(2) || m["ready_pods"] != int32(1) {
		t.Errorf("StatefulSet conversion: %+v", m)
	}
	if m["service_key"] != "shop/StatefulSet/db" {
		t.Errorf("service_key = %v", m["service_key"])
	}
}

func TestConvertDaemonSet(t *testing.T) {
	d := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "node-exporter", Namespace: "monitoring"},
		Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 5, NumberReady: 4},
	}
	got, _ := convertDaemonSet(d)
	m := got.(map[string]any)
	if m["total_pods"] != int32(5) || m["ready_pods"] != int32(4) {
		t.Errorf("DaemonSet counts: total=%v ready=%v", m["total_pods"], m["ready_pods"])
	}
}

func TestConvertNode(t *testing.T) {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", Labels: map[string]string{"role": "worker"}},
		Spec: corev1.NodeSpec{
			Unschedulable: true,
			Taints:        []corev1.Taint{{Key: "k", Value: "v", Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("8Gi"),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("3.5"),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
			},
			NodeInfo: corev1.NodeSystemInfo{
				Architecture:    "arm64",
				KubeletVersion:  "v1.32.0",
				KernelVersion:   "6.1.0",
				OSImage:         "Ubuntu 22.04",
				OperatingSystem: "linux",
			},
		},
	}
	got, _ := convertNode(n)
	m := got.(map[string]any)
	// Wire shape: the collector accesses k8s_data["node_creation_time"],
	// "memory_capacity", "cpu_capacity", "node_info" by name.
	if _, ok := m["node_creation_time"].(string); !ok {
		t.Errorf("node_creation_time missing or wrong type: %T", m["node_creation_time"])
	}
	if m["memory_capacity"].(int64) != 8*1024 { // 8 GiB → 8192 MiB
		t.Errorf("memory_capacity = %v MB; want 8192", m["memory_capacity"])
	}
	if m["cpu_capacity"].(float64) != 4.0 {
		t.Errorf("cpu_capacity = %v cores; want 4", m["cpu_capacity"])
	}
	if m["cpu_allocatable"].(float64) != 3.5 {
		t.Errorf("cpu_allocatable = %v cores; want 3.5", m["cpu_allocatable"])
	}
	// taints + conditions are emitted as comma-joined strings, not lists.
	if !strings.Contains(m["conditions"].(string), "Ready:True") {
		t.Errorf("conditions = %v; want substring Ready:True", m["conditions"])
	}
	if !strings.Contains(m["taints"].(string), "k=v:NoSchedule") {
		t.Errorf("taints = %v; want substring k=v:NoSchedule", m["taints"])
	}
	info, _ := m["node_info"].(map[string]any)
	system, _ := info["system"].(map[string]any)
	if system["kubelet_version"] != "v1.32.0" {
		t.Errorf("node_info.system.kubelet_version = %v", system["kubelet_version"])
	}
}

func TestConvertNamespace(t *testing.T) {
	n := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "shop",
			Labels:      map[string]string{"team": "frontend"},
			Annotations: map[string]string{"x": "y"},
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	got, _ := convertNamespace(n)
	m := got.(map[string]any)
	if m["name"] != "shop" || m["phase"] != "Active" {
		t.Errorf("ns = %v", m)
	}
}

func TestConvertX_RejectsWrongType(t *testing.T) {
	cases := []func(any) (any, bool){
		convertDeployment, convertStatefulSet, convertDaemonSet, convertNode,
		convertNamespace, convertPod, convertReplicaSet, convertJob, convertCronJob,
	}
	for _, fn := range cases {
		if _, ok := fn(&corev1.Service{}); ok {
			t.Errorf("converter accepted wrong type")
		}
	}
}

func TestConvertReplicaSet_FiltersZeroReplicas(t *testing.T) {
	zero := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "n"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(0))},
	}
	if _, ok := convertReplicaSet(zero); ok {
		t.Error("ReplicaSet with replicas=0 should be filtered out")
	}
	live := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "n"},
		Spec:       appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(3))},
		Status:     appsv1.ReplicaSetStatus{ReadyReplicas: 3},
	}
	got, ok := convertReplicaSet(live)
	if !ok {
		t.Fatal("expected live RS to be emitted")
	}
	m := got.(map[string]any)
	if m["type"] != "ReplicaSet" || m["total_pods"] != int32(3) {
		t.Errorf("rs = %v", m)
	}
	if m["service_key"] != "n/ReplicaSet/live" {
		t.Errorf("service_key = %v", m["service_key"])
	}
}

// TestConvertJob_BasicShape asserts the JobInfo wire shape — the collector's
// job branch reads service_key, created_at, updated_at, status (dict),
// job_data (dict), cpu_req, mem_req, completions directly
// . status.succeeded must round-trip through
// the `status` map, not be a top-level field.
func TestConvertJob_BasicShape(t *testing.T) {
	j := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "import", Namespace: "etl"},
		Spec:       batchv1.JobSpec{Completions: ptr.To(int32(1))},
		Status:     batchv1.JobStatus{Succeeded: 1, Failed: 0, Active: 0},
	}
	got, ok := convertJob(j)
	if !ok {
		t.Fatal("expected Job to convert")
	}
	m := got.(map[string]any)
	if m["completions"] != int32(1) {
		t.Errorf("completions = %v", m["completions"])
	}
	if m["service_key"] != "etl/Job/import" {
		t.Errorf("service_key = %v", m["service_key"])
	}
	if _, ok := m["updated_at"].(int64); !ok {
		t.Errorf("updated_at should be int64 millis; got %T", m["updated_at"])
	}
	if _, ok := m["created_at"].(string); !ok {
		t.Errorf("created_at should be string (str(metadata.creation_timestamp))")
	}
	status, _ := m["status"].(map[string]any)
	if status["succeeded"] != int32(1) {
		t.Errorf("status.succeeded = %v", status["succeeded"])
	}
	if _, ok := m["job_data"].(map[string]any); !ok {
		t.Errorf("job_data missing; collector reads it at handler line 1410")
	}
}

func TestConvertCronJob_BasicShape(t *testing.T) {
	c := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "etl"},
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * *",
			Suspend:  ptr.To(false),
		},
	}
	got, ok := convertCronJob(c)
	if !ok {
		t.Fatal("expected CronJob to convert")
	}
	m := got.(map[string]any)
	if m["type"] != "CronJob" || m["service_key"] != "etl/CronJob/nightly" {
		t.Errorf("cron type/service_key = %v / %v", m["type"], m["service_key"])
	}
	jd, _ := m["job_data"].(map[string]any)
	if jd["schedule"] != "0 0 * * *" {
		t.Errorf("job_data.schedule = %v", jd["schedule"])
	}
}

func TestIsHelmRelease(t *testing.T) {
	cases := []struct {
		labels      map[string]string
		annotations map[string]string
		want        bool
	}{
		{nil, nil, false},
		{map[string]string{"app.kubernetes.io/managed-by": "Helm"}, nil, true},
		{map[string]string{"app.kubernetes.io/managed-by": "Argo"}, nil, false},
		{map[string]string{"helm.sh/chart": "x"}, nil, true},
		{nil, map[string]string{"meta.helm.sh/release-name": "x"}, true},
	}
	for _, c := range cases {
		if got := isHelmRelease(c.labels, c.annotations); got != c.want {
			t.Errorf("isHelmRelease(%v,%v) = %v; want %v", c.labels, c.annotations, got, c.want)
		}
	}
}

// Regression: the legacy emitter shipped 11 keys under meta.config; the
// initial Go port shipped only 3 (labels, containers, owner). The UI's
// KubernetesWorkloads drilldown crashed in `volumes.length` because the
// key was undefined. Confirm the workload converter emits all 11 keys.
func TestConvertDeployment_EmitsAll11ConfigKeys(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "frontend",
			Namespace:   "shop",
			Annotations: map[string]string{"team": "search"},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "frontend"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:         []corev1.Container{{Name: "c", Image: "nginx"}},
					ServiceAccountName: "frontend-sa",
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-pvc"},
						}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
					Tolerations: []corev1.Toleration{
						{Key: "spot", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					},
					Affinity: &corev1.Affinity{
						NodeAffinity: &corev1.NodeAffinity{
							PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{Weight: 1}},
						},
					},
				},
			},
		},
	}
	got, ok := convertDeployment(d)
	if !ok {
		t.Fatal("convertDeployment ok=false")
	}
	cfg := got.(map[string]any)["config"].(map[string]any)

	want := []string{
		"labels", "containers", "owner",
		"volumes", "toleration", "affinity",
		"annotations", "service_account",
		"qos_class", "ip", "conditions",
	}
	for _, k := range want {
		if _, has := cfg[k]; !has {
			t.Errorf("config[%q] missing — UI crashes when fields are undefined", k)
		}
	}

	// Volumes shape: PVC volume carries persistent_volume_claim.claim_name;
	// EmptyDir / others carry only the name.
	vols := cfg["volumes"].([]map[string]any)
	if len(vols) != 2 {
		t.Fatalf("volumes length = %d; want 2", len(vols))
	}
	pvc, hasPVC := vols[0]["persistent_volume_claim"].(map[string]any)
	if !hasPVC || pvc["claim_name"] != "data-pvc" {
		t.Errorf("PVC volume missing claim_name; got %v", vols[0])
	}
	if _, has := vols[1]["persistent_volume_claim"]; has {
		t.Errorf("EmptyDir volume must NOT carry persistent_volume_claim; got %v", vols[1])
	}

	// service_account / annotations / toleration should be populated.
	if cfg["service_account"] != "frontend-sa" {
		t.Errorf("service_account = %v", cfg["service_account"])
	}
	if ann := cfg["annotations"].(map[string]string); ann["team"] != "search" {
		t.Errorf("annotations[team] = %v", ann)
	}
	tols := cfg["toleration"].([]map[string]any)
	if len(tols) != 1 || tols[0]["key"] != "spot" {
		t.Errorf("toleration = %v", cfg["toleration"])
	}

	// qos_class / ip / conditions stay nil/empty when no lookup is wired
	// (test path). The Pod-lookup branch is covered separately below.
	if cfg["qos_class"] != nil {
		t.Errorf("qos_class without lookup should be nil; got %v", cfg["qos_class"])
	}
	if cfg["ip"] != nil {
		t.Errorf("ip without lookup should be nil; got %v", cfg["ip"])
	}
	if conds := cfg["conditions"].([]map[string]any); len(conds) != 0 {
		t.Errorf("conditions without lookup should be empty; got %v", conds)
	}
}

// Regression: with the Go-agent cutover, Deployment-owned pods were
// being emitted with owner=[{ReplicaSet, <hash-named-rs>}] because the
// converter forwarded raw OwnerReferences. The collector keys
// k8s_pods.workload_{type,name} off owner[0], so every UI/triage query
// filtered by workload_type="Deployment" started returning empty. The
// fix: when the immediate owner is a ReplicaSet, walk one hop via the
// RS informer and emit the RS's controller (the Deployment) instead.
//
// This authoritative lookup avoids the legacy regex heuristic
// (strip-pod-template-hash) used by trigger fingerprinting — works
// correctly for RSes created outside the Deployment controller
// (manual / operator-owned RSes whose names don't end in a hash) and
// doesn't break if the controller's hash format ever changes.
func TestPodConverter_ResolvesReplicaSetOwnerToDeployment(t *testing.T) {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-7f9d8c5b6",
			Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "Deployment",
				Name:       "checkout",
				Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Replicas: ptr.To(int32(1))},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "checkout-7f9d8c5b6-w8pqw",
			Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{
				Kind:       "ReplicaSet",
				Name:       "checkout-7f9d8c5b6",
				Controller: ptr.To(true),
			}},
		},
	}

	lookup := func(ns, name string) *appsv1.ReplicaSet {
		if ns == "shop" && name == "checkout-7f9d8c5b6" {
			return rs
		}
		return nil
	}

	got, ok := newPodConverter(lookup)(pod)
	if !ok {
		t.Fatal("converter ok=false")
	}
	cfg := got.(map[string]any)["config"].(map[string]any)
	owners := cfg["owner"].([]map[string]any)
	if len(owners) != 1 {
		t.Fatalf("owners = %v; want one resolved entry", owners)
	}
	if owners[0]["kind"] != "Deployment" || owners[0]["name"] != "checkout" {
		t.Errorf("owners[0] = %v; want {kind: Deployment, name: checkout}", owners[0])
	}
}

// When the RS isn't (yet) in cache, the converter must fall back to
// emitting the immediate ReplicaSet owner unchanged. The next
// pod-status event self-heals once WaitForCacheSync completes. Without
// this fallback, a startup race could drop pods entirely.
func TestPodConverter_FallsBackWhenReplicaSetMissing(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "p",
			Namespace: "shop",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "ReplicaSet", Name: "missing-rs",
			}},
		},
	}
	got, ok := newPodConverter(func(string, string) *appsv1.ReplicaSet { return nil })(pod)
	if !ok {
		t.Fatal("converter ok=false")
	}
	cfg := got.(map[string]any)["config"].(map[string]any)
	owners := cfg["owner"].([]map[string]any)
	if len(owners) != 1 || owners[0]["kind"] != "ReplicaSet" || owners[0]["name"] != "missing-rs" {
		t.Errorf("fallback owners = %v; want immediate RS unchanged", owners)
	}
}

// Non-Deployment workloads (DaemonSet, StatefulSet, Job, operator CRs)
// own pods directly. The converter must NOT touch those owner refs.
func TestPodConverter_PassesThroughNonReplicaSetOwners(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "node-exporter-abc",
			Namespace: "kube-system",
			OwnerReferences: []metav1.OwnerReference{{
				Kind: "DaemonSet", Name: "node-exporter",
			}},
		},
	}
	// Provide a lookup that would panic if invoked — proves the
	// converter short-circuits on non-RS kinds.
	lookup := func(string, string) *appsv1.ReplicaSet {
		t.Fatal("rsLookup should not be called for non-ReplicaSet owners")
		return nil
	}
	got, ok := newPodConverter(lookup)(pod)
	if !ok {
		t.Fatal("converter ok=false")
	}
	cfg := got.(map[string]any)["config"].(map[string]any)
	owners := cfg["owner"].([]map[string]any)
	if len(owners) != 1 || owners[0]["kind"] != "DaemonSet" || owners[0]["name"] != "node-exporter" {
		t.Errorf("daemonset passthrough owners = %v", owners)
	}
}

// When a Pod owned by the workload has reported status, qos_class / ip
// / conditions come from that Pod.
func TestServiceDict_PodLookupFillsRuntimeFields(t *testing.T) {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "nginx"}}},
			},
		},
	}
	livePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Labels:    map[string]string{"app": "api"},
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSBurstable,
			PodIP:    "10.42.7.3",
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			},
		},
	}
	lookup := func(ns string, _ labels.Selector) *corev1.Pod {
		if ns != "ns" {
			return nil
		}
		return livePod
	}

	got, ok := newDeploymentConverter(lookup)(d)
	if !ok {
		t.Fatal("converter ok=false")
	}
	cfg := got.(map[string]any)["config"].(map[string]any)
	if cfg["qos_class"] != "Burstable" {
		t.Errorf("qos_class = %v; want Burstable", cfg["qos_class"])
	}
	if cfg["ip"] != "10.42.7.3" {
		t.Errorf("ip = %v", cfg["ip"])
	}
	conds := cfg["conditions"].([]map[string]any)
	if len(conds) != 2 || conds[0]["type"] != "Ready" || conds[0]["status"] != "True" {
		t.Errorf("conditions = %v", conds)
	}
}
