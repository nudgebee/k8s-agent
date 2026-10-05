package telemetry

import (
	"context"
	"errors"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestCanUpdatePrometheusRulesClusterWide(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		err     error
		want    bool
	}{
		{"allowed", true, nil, true},
		{"denied", false, nil, false},
		{"api error", true, errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewClientset()
			var got *authorizationv1.SelfSubjectAccessReview
			kube.PrependReactor("create", "selfsubjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
				got = a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
				if tc.err != nil {
					return true, nil, tc.err
				}
				got.Status.Allowed = tc.allowed
				return true, got, nil
			})
			if res := CanUpdatePrometheusRulesClusterWide(context.Background(), kube, nil); res != tc.want {
				t.Fatalf("got %v, want %v", res, tc.want)
			}
			attrs := got.Spec.ResourceAttributes
			if attrs.Group != "monitoring.coreos.com" || attrs.Resource != "prometheusrules" || attrs.Verb != "update" || attrs.Namespace != "" {
				t.Errorf("wrong review attributes: %+v", attrs)
			}
		})
	}
	if CanUpdatePrometheusRulesClusterWide(context.Background(), nil, nil) {
		t.Error("nil client must report false")
	}
}
