package servicemap

import (
	"encoding/json"
	"testing"
)

func TestParsePromRangeResponse_HappyPath(t *testing.T) {
	raw := json.RawMessage(`{
		"status":"success",
		"data":{
			"resultType":"matrix",
			"result":[
				{"metric":{"job":"prometheus","instance":"a"},"values":[[100,"1.5"],[200,"2.5"]]},
				{"metric":{"job":"prometheus","instance":"b"},"values":[[100,"NaN"]]}
			]
		}
	}`)
	got, err := parsePromResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if !got[0].HasVal || got[0].Last != 2.5 {
		t.Errorf("first.last = %v; want 2.5", got[0].Last)
	}
	if got[0].Metric["instance"] != "a" {
		t.Errorf("first.instance = %s", got[0].Metric["instance"])
	}
	if got[1].HasVal {
		t.Errorf("NaN sample should have no value, got %v", got[1].Last)
	}
}

func TestParsePromRangeResponse_EmptyAndError(t *testing.T) {
	if got, err := parsePromResponse(nil); err != nil || got != nil {
		t.Errorf("nil input: got=%v err=%v", got, err)
	}
	if got, err := parsePromResponse([]byte(`{"status":"error","data":{}}`)); err != nil || got != nil {
		t.Errorf("error status should produce nil, got %v %v", got, err)
	}
	if _, err := parsePromResponse([]byte(`not json`)); err == nil {
		t.Error("expected JSON parse error")
	}
}

func TestLastValue_BadInput(t *testing.T) {
	if _, ok := lastValue(nil); ok {
		t.Error("nil values should not parse")
	}
	if _, ok := lastValue([][]any{{100}}); ok {
		t.Error("malformed pair should not parse")
	}
	if _, ok := lastValue([][]any{{100, 42}}); ok {
		t.Error("non-string value should not parse")
	}
	for _, s := range []string{"NaN", "+Inf", "-Inf"} {
		if _, ok := lastValue([][]any{{100, s}}); ok {
			t.Errorf("%s should not count as a value", s)
		}
	}
}

func TestLabelOr(t *testing.T) {
	if labelOr(map[string]string{"k": "v"}, "k", "x") != "v" {
		t.Error("labelOr should return present value")
	}
	if labelOr(map[string]string{}, "k", "fallback") != "fallback" {
		t.Error("labelOr should fall back on missing key")
	}
	if labelOr(map[string]string{"k": ""}, "k", "fallback") != "fallback" {
		t.Error("labelOr should fall back on empty value")
	}
}

// One non-finite series on an edge must not wipe out the others' sum.
func TestBuild_NonFiniteSampleIgnored(t *testing.T) {
	raw := json.RawMessage(`{"status":"success","data":{"resultType":"vector","result":[
		{"metric":{"src_workload_kind":"Deployment","src_workload_name":"api","src_workload_namespace":"shop","destination_workload_kind":"Deployment","destination_workload_name":"db","destination_workload_namespace":"shop","status":"200"},"value":[1,"12"]},
		{"metric":{"src_workload_kind":"Deployment","src_workload_name":"api","src_workload_namespace":"shop","destination_workload_kind":"Deployment","destination_workload_name":"db","destination_workload_namespace":"shop","status":"500"},"value":[1,"+Inf"]}
	]}}`)
	results, err := parsePromResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	w := build(map[string][]promResult{"l7_requests:HTTP": results})
	la := w.edges[appKey(ApplicationID{Name: "api", Kind: "Deployment", Namespace: "shop"})][appKey(ApplicationID{Name: "db", Kind: "Deployment", Namespace: "shop"})]
	if la == nil || la.requestRate() != 12 || la.failures != 0 {
		t.Errorf("edge = %+v; want 12 requests and no failures", la)
	}
}
