package podexec

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// ---------- target pod gone ----------

func replicaSetFor(name, deployment string, labels map[string]string) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop"},
		Spec:       appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
	}
	if deployment != "" {
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: deployment, UID: "dep-uid", Controller: ptr.To(true),
		}}
	}
	return rs
}

func podWith(name string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Labels: labels},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestProfile_TargetGone(t *testing.T) {
	appLabels := map[string]string{"app": "web"}
	oldRS := map[string]string{"app": "web", "pod-template-hash": "6d8f7c9b4"}
	newRS := map[string]string{"app": "web", "pod-template-hash": "58c9d7f6b"}
	terminating := podWith("web-6d8f7c9b4-zzzzz", oldRS, corev1.PodRunning)
	terminating.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	terminating.Finalizers = []string{"example.com/hold"}

	cases := []struct {
		name    string
		pod     string
		objects []runtime.Object
		forbid  bool
		want    string
	}{
		{
			// The usual stale name: a rollout scaled the old ReplicaSet to
			// zero, the Deployment's pods now come from the new one.
			name: "rolled deployment lists the replacements",
			pod:  "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{
				replicaSetFor("web-6d8f7c9b4", "web", oldRS),
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
					Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: appLabels}},
				},
				podWith("web-58c9d7f6b-p9q8z", newRS, corev1.PodPending),
				podWith("web-58c9d7f6b-b7n4m", newRS, corev1.PodRunning),
				terminating,
				podWith("other-0", map[string]string{"app": "other"}, corev1.PodRunning),
			},
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; deployment web currently runs " +
				"web-58c9d7f6b-b7n4m (Running), web-58c9d7f6b-p9q8z (Pending)",
		},
		{
			name: "bare replicaset lists its own pods",
			pod:  "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{
				replicaSetFor("web-6d8f7c9b4", "", oldRS),
				podWith("web-6d8f7c9b4-k5l6m", oldRS, corev1.PodRunning),
			},
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; replicaset web-6d8f7c9b4 currently runs " +
				"web-6d8f7c9b4-k5l6m (Running)",
		},
		{
			name:    "replicaset with no pods says so",
			pod:     "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", oldRS)},
			want:    "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists; replicaset web-6d8f7c9b4 currently has no pods",
		},
		{
			name: "replicaset gone too",
			pod:  "web-6d8f7c9b4-x2v4k",
			want: "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists",
		},
		{
			name: "statefulset-style name gets no lookup",
			pod:  "db-0",
			want: "pod_profiler: pod shop/db-0 no longer exists",
		},
		{
			name:    "rbac denial degrades to the plain message",
			pod:     "web-6d8f7c9b4-x2v4k",
			objects: []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", oldRS)},
			forbid:  true,
			want:    "pod_profiler: pod shop/web-6d8f7c9b4-x2v4k no longer exists",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.objects...)
			if tc.forbid {
				cs.PrependReactor("get", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "web-6d8f7c9b4", nil)
				})
			}
			h := NewProfilerHandler(cs, fakeRestConfig)
			_, err := h.Profile(context.Background(), ProfileRequest{Name: tc.pod, Namespace: "shop", Lang: LangGo})
			if err == nil || err.Error() != tc.want {
				t.Errorf("err = %v\nwant  %s", err, tc.want)
			}
		})
	}
}

// TestLivePodsHint_CapsList — a large Deployment lists a handful of pods,
// not all of them.
func TestLivePodsHint_CapsList(t *testing.T) {
	labels := map[string]string{"app": "web"}
	objects := []runtime.Object{replicaSetFor("web-6d8f7c9b4", "", labels)}
	for _, n := range []string{"bbbbb", "ccccc", "ddddd", "fffff", "ggggg", "hhhhh", "jjjjj"} {
		objects = append(objects, podWith("web-6d8f7c9b4-"+n, labels, corev1.PodRunning))
	}
	h := NewProfilerHandler(fake.NewClientset(objects...), fakeRestConfig)
	hint := h.livePodsHint(context.Background(), "shop", "web-6d8f7c9b4-x2v4k")
	if strings.Count(hint, "(Running)") != maxLivePods || !strings.HasSuffix(hint, " and 2 more") {
		t.Errorf("hint = %q", hint)
	}
}
