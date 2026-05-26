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
	got, err := parsePromRangeResponse(raw)
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
}

func TestParsePromRangeResponse_EmptyAndError(t *testing.T) {
	if got, err := parsePromRangeResponse(nil); err != nil || got != nil {
		t.Errorf("nil input: got=%v err=%v", got, err)
	}
	if got, err := parsePromRangeResponse([]byte(`{"status":"error","data":{}}`)); err != nil || got != nil {
		t.Errorf("error status should produce nil, got %v %v", got, err)
	}
	if _, err := parsePromRangeResponse([]byte(`not json`)); err == nil {
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
