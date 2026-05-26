package telemetry

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func deployment(namespace, name, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: image}},
				},
			},
		},
	}
}

func TestDetectAutoScaler(t *testing.T) {
	cases := []struct {
		name     string
		objs     []runtime.Object
		provider string
		want     AutoScalerInfo
	}{
		{
			name: "Karpenter installed",
			objs: []runtime.Object{deployment("karpenter", "karpenter", "ghcr.io/karpenter:v0.32.1")},
			want: AutoScalerInfo{Enabled: true, Type: "karpenter", Version: "v0.32.1", Namespace: "karpenter"},
		},
		{
			name: "Cluster Autoscaler installed",
			objs: []runtime.Object{deployment("kube-system", "cluster-autoscaler", "k8s.gcr.io/autoscaling/cluster-autoscaler:v1.27.3")},
			want: AutoScalerInfo{Enabled: true, Type: "cluster-autoscaler", Version: "v1.27.3", Namespace: "kube-system"},
		},
		{
			name:     "GKE provider with no autoscaler deployment",
			objs:     nil,
			provider: providerGKE,
			want:     AutoScalerInfo{Enabled: true, Type: "gke"},
		},
		{
			name: "No autoscaler, non-GKE",
			objs: []runtime.Object{deployment("default", "nginx", "nginx:1.25")},
			want: AutoScalerInfo{},
		},
		{
			name: "Karpenter image with digest — version empty",
			objs: []runtime.Object{deployment("karpenter", "karpenter-controller",
				"public.ecr.aws/karpenter/controller@sha256:deadbeef")},
			want: AutoScalerInfo{Enabled: true, Type: "karpenter", Namespace: "karpenter"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs := fake.NewClientset(tc.objs...)
			got := DetectAutoScaler(context.Background(), cs, tc.provider, nil)
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
