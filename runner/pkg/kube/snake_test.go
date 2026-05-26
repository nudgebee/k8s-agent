package kube

import (
	"reflect"
	"testing"
)

func TestToSnakeCase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"name", "name"},
		{"creationTimestamp", "creation_timestamp"},
		{"accessModes", "access_modes"},
		{"persistentVolumeReclaimPolicy", "persistent_volume_reclaim_policy"},
		{"storageClassName", "storage_class_name"},
		// Acronyms: "APIVersion" → split before the lowercase that follows the
		// last uppercase of the run.
		{"APIVersion", "api_version"},
		{"hostIP", "host_ip"},
		{"podIP", "pod_ip"},
		// Already snake or non-schema → passthrough.
		{"already_snake", "already_snake"},
		{"app.kubernetes.io/name", "app.kubernetes.io/name"},
		{"kubectl.kubernetes.io/last-applied-configuration", "kubectl.kubernetes.io/last-applied-configuration"},
		// Digits separate words like a lowercase letter does.
		{"v1ResourceQuota", "v1_resource_quota"},
	}
	for _, tc := range cases {
		got := toSnakeCase(tc.in)
		if got != tc.want {
			t.Errorf("toSnakeCase(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestSnakeKeysDeep_Basic(t *testing.T) {
	in := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolume",
		"metadata": map[string]any{
			"name":              "pv-1",
			"creationTimestamp": "2024-01-01T00:00:00Z",
			"resourceVersion":   "12345",
		},
		"spec": map[string]any{
			"accessModes":                   []any{"ReadWriteOnce"},
			"storageClassName":              "gp2",
			"persistentVolumeReclaimPolicy": "Delete",
		},
	}
	got := SnakeKeysDeep(in).(map[string]any)

	meta := got["metadata"].(map[string]any)
	if _, ok := meta["creationTimestamp"]; ok {
		t.Error("camelCase key creationTimestamp leaked through")
	}
	if meta["creation_timestamp"] != "2024-01-01T00:00:00Z" {
		t.Errorf("creation_timestamp = %v; want timestamp", meta["creation_timestamp"])
	}

	spec := got["spec"].(map[string]any)
	if spec["storage_class_name"] != "gp2" {
		t.Errorf("storage_class_name = %v; want gp2", spec["storage_class_name"])
	}
	if spec["persistent_volume_reclaim_policy"] != "Delete" {
		t.Errorf("persistent_volume_reclaim_policy = %v", spec["persistent_volume_reclaim_policy"])
	}
	am := spec["access_modes"].([]any)
	if len(am) != 1 || am[0] != "ReadWriteOnce" {
		t.Errorf("access_modes = %v", am)
	}
}

func TestSnakeKeysDeep_PreservesUserDataKeys(t *testing.T) {
	// labels, annotations, configmap.data, etc. carry user-supplied keys
	// like "app.kubernetes.io/name" which must NOT be snake-cased.
	in := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{
				"app.kubernetes.io/name":    "frontend",
				"app.kubernetes.io/version": "1.2.3",
			},
			"annotations": map[string]any{
				"kubectl.kubernetes.io/last-applied-configuration": "{...}",
				"someAnnotation": "ok-but-rare",
			},
		},
		"data": map[string]any{
			"my.config.file":   "value=1",
			"AnotherKey.Camel": "raw",
		},
	}
	got := SnakeKeysDeep(in).(map[string]any)
	meta := got["metadata"].(map[string]any)

	labels := meta["labels"].(map[string]any)
	if _, ok := labels["app.kubernetes.io/name"]; !ok {
		t.Error("labels key app.kubernetes.io/name was not preserved")
	}
	if labels["app.kubernetes.io/name"] != "frontend" {
		t.Errorf("labels value lost: %v", labels)
	}

	annotations := meta["annotations"].(map[string]any)
	if _, ok := annotations["kubectl.kubernetes.io/last-applied-configuration"]; !ok {
		t.Error("annotations key kubectl.kubernetes.io/last-applied-configuration was not preserved")
	}
	// User-data containers don't snake-case their own keys, even if they
	// look camelCase — these are user-chosen names that must round-trip.
	if _, ok := annotations["someAnnotation"]; !ok {
		t.Error("annotations should preserve raw user-supplied keys, even camelCase ones")
	}

	data := got["data"].(map[string]any)
	if _, ok := data["my.config.file"]; !ok {
		t.Error("configmap data key my.config.file was not preserved")
	}
	if _, ok := data["AnotherKey.Camel"]; !ok {
		t.Error("configmap data key AnotherKey.Camel was not preserved (.has-period passthrough should still apply but parent skip is the safer guarantee)")
	}
}

func TestSnakeKeysDeep_Selector_MatchLabels(t *testing.T) {
	// selector.matchLabels itself has a snake-cased path
	// (selector → match_labels) but its VALUES are user labels.
	in := map[string]any{
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"app.kubernetes.io/name": "x",
				},
			},
		},
	}
	got := SnakeKeysDeep(in).(map[string]any)
	spec := got["spec"].(map[string]any)
	sel := spec["selector"].(map[string]any)
	if _, ok := sel["match_labels"]; !ok {
		t.Fatalf("matchLabels was not renamed to match_labels: %v", sel)
	}
	ml := sel["match_labels"].(map[string]any)
	if _, ok := ml["app.kubernetes.io/name"]; !ok {
		t.Errorf("match_labels lost user key: %v", ml)
	}
}

func TestSnakeKeysDeep_NestedListOfPodSpec(t *testing.T) {
	// Pod spec.containers[].volumeMounts[].mountPath is a deeply nested
	// camelCase path that must convert end-to-end.
	in := map[string]any{
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  "c",
					"image": "nginx:latest",
					"volumeMounts": []any{
						map[string]any{
							"mountPath": "/data",
							"name":      "vol",
						},
					},
					"imagePullPolicy": "IfNotPresent",
				},
			},
		},
	}
	got := SnakeKeysDeep(in).(map[string]any)
	spec := got["spec"].(map[string]any)
	containers := spec["containers"].([]any)
	c := containers[0].(map[string]any)
	if c["image_pull_policy"] != "IfNotPresent" {
		t.Errorf("image_pull_policy = %v", c["image_pull_policy"])
	}
	mounts := c["volume_mounts"].([]any)
	mt := mounts[0].(map[string]any)
	if mt["mount_path"] != "/data" {
		t.Errorf("mount_path = %v", mt["mount_path"])
	}
}

func TestSnakeKeysDeep_ScalarsPassThrough(t *testing.T) {
	cases := []any{nil, "string", 42, true, 3.14, []any{1, 2, 3}}
	for _, in := range cases {
		got := SnakeKeysDeep(in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("scalar %v mutated to %v", in, got)
		}
	}
}
