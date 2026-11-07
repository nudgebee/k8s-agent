package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every command here reaches Secret values or a local credential file. Each must
// be refused in read-only AND write mode — write RBAC is not a Secret grant.
func TestKubectl_RefusesSecretAccess(t *testing.T) {
	cmds := []string{
		// Secret-bearing kinds, every spelling.
		"kubectl get secret",
		"kubectl get secrets -A",
		"kubectl get Secrets -n app",
		"kubectl get secret/db-creds -n app -o yaml",
		"kubectl get secrets.v1. -n app",
		"kubectl get configmaps,secrets -n app",
		"kubectl get pods/foo,secrets/bar",
		"kubectl -n app get secret db -o jsonpath='{.data.password}'",
		"kubectl --context=ctx get secret db",
		"kubectl describe secret db",
		"kubectl get secret db -o yaml && kubectl get pods",
		"kubectl get pods; kubectl get secret db",
		"kubectl get sealedsecrets -A",
		"kubectl get externalsecrets -A",
		"kubectl get es -A",
		"kubectl get clustersecretstores",
		"kubectl get ss -n app",
		"kubectl get secretproviderclasses -A",
		"kubectl get spc -A",
		"kubectl explain secret.data",
		// --raw paths to secrets, including a percent-encoded one.
		"kubectl get --raw /api/v1/namespaces/app/secrets/db",
		"kubectl get --raw=/api/v1/secrets",
		"kubectl get --raw /api/v1/watch/secrets?watch=1",
		"kubectl get --raw /api/v1/namespaces/app/%73ecrets",
		"kubectl get --raw /apis/external-secrets.io/v1beta1/externalsecrets",
		"kubectl create --raw /api/v1/namespaces/app/serviceaccounts/default/token -f req.json",
		// File-backed output formats read the agent pod's filesystem.
		"kubectl get pods -o jsonpath-file=/var/run/secrets/kubernetes.io/serviceaccount/token",
		"kubectl get pods -o jsonpath-file=/tmp/t",
		"kubectl get pods -ojsonpath-file=/tmp/t",
		"kubectl get pods -o=go-template-file=/tmp/t",
		"kubectl get pods --output=custom-columns-file=/tmp/t",
		"kubectl get pods --output templatefile --template=/tmp/t",
		// Mounted credential paths.
		"kubectl get -f /var/run/secrets/kubernetes.io/serviceaccount/token",
		"kubectl get pods --token-file=/var/run/../run/secrets/x",
	}
	for _, write := range []bool{false, true} {
		k := &KubectlExecutor{BinaryPath: "/usr/bin/true", AllowWrite: write}
		for _, cmd := range cmds {
			if _, err := k.Run(context.Background(), cmd); err == nil {
				t.Errorf("AllowWrite=%v %q: ran; want refused", write, cmd)
			}
		}
	}
}

// Write mode must not create, change or delete Secrets, or mint SA tokens.
func TestKubectl_WriteModeRefusesSecretMutations(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true", AllowWrite: true}
	for _, cmd := range []string{
		"kubectl create secret generic x --from-literal=a=b",
		"kubectl delete secret x",
		"kubectl patch secret x -p '{}'",
		"kubectl edit secret x",
		"kubectl create token default -n app",
	} {
		if _, err := k.Run(context.Background(), cmd); err == nil {
			t.Errorf("%q: ran; want refused", cmd)
		}
	}
}

// Names that merely contain "secret", and inline templates, stay usable.
func TestKubectl_SecretPolicyAllowsNonSecretReads(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	for _, cmd := range []string{
		"kubectl get pods",
		"kubectl get pod secret-rotator -n app",
		"kubectl get deploy my-secret-app -o yaml",
		"kubectl get certificate tls-secret -n traefik -o yaml",
		"kubectl get pods -o jsonpath='{.items[*].spec.imagePullSecrets}'",
		"kubectl get pods -o custom-columns=NAME:.metadata.name",
		"kubectl get pods -o go-template='{{.metadata.name}}'",
		"kubectl get --raw /api/v1/namespaces/app/pods",
		"kubectl get --raw /metrics",
		"kubectl auth can-i get secrets",
		"kubectl logs deploy/api -n app",
	} {
		if _, err := k.Run(context.Background(), cmd); err != nil {
			t.Errorf("%q: refused: %v", cmd, err)
		}
	}
}

func TestKubectl_SecretRefusalNamesTheKind(t *testing.T) {
	k := &KubectlExecutor{BinaryPath: "/usr/bin/true"}
	_, err := k.Run(context.Background(), "kubectl get configmaps,secrets -n app")
	if err == nil || !strings.Contains(err.Error(), `"secrets"`) {
		t.Fatalf("err = %v; want it to name \"secrets\"", err)
	}
}

func TestGetResource_RedactsSecretValues(t *testing.T) {
	sec := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: "app",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"password":"aHVudGVyMg=="}}`,
				"team": "payments",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"password": []byte("hunter2"), "user": []byte("app")},
	}
	c := newFakeDynamic(t, sec)

	got, err := c.GetResource(context.Background(), GetParams{Version: "v1", ResourceType: "secrets", Namespace: "app"})
	if err != nil {
		t.Fatal(err)
	}
	arr := got.([]any)
	if len(arr) != 1 {
		t.Fatalf("len = %d; want 1", len(arr))
	}
	obj := arr[0].(map[string]any)
	data, _ := obj["data"].(map[string]any)
	if len(data) != 2 {
		t.Fatalf("data keys = %v; want password and user kept", data)
	}
	for k, v := range data {
		if v != "" {
			t.Errorf("data[%q] = %v; want blank", k, v)
		}
	}
	ann := obj["metadata"].(map[string]any)["annotations"].(map[string]any)
	if _, ok := ann["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("last-applied-configuration kept; it carries the values")
	}
	if ann["team"] != "payments" {
		t.Errorf("unrelated annotation lost: %v", ann)
	}

	y, err := c.GetResourceYAML(context.Background(), GetParams{Version: "v1", ResourceType: "secrets", Namespace: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(y), "aHVudGVyMg") {
		t.Errorf("YAML still carries the secret value:\n%s", y)
	}
}
