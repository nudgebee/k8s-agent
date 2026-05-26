package enrichers

import (
	"encoding/json"
	"testing"
)

// TestPrometheusQueryResultDict checks each result_type round-trips through
// the same shape PrometheusQueryResult.dict() produces. These wire
// shapes feed directly into PrometheusBlock.data, which api-server
// walks via vector_result / series_list_result / scalar_result.
func TestPrometheusQueryResultDict(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		assert func(*testing.T, map[string]any)
	}{
		{
			name: "vector",
			body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"job":"x"},"value":[1700000000,"1.5"]}]}}`,
			assert: func(t *testing.T, d map[string]any) {
				if d["result_type"] != "vector" {
					t.Errorf("result_type = %v", d["result_type"])
				}
				vr, ok := d["vector_result"].([]map[string]any)
				if !ok || len(vr) != 1 {
					t.Fatalf("vector_result missing/empty: %T %v", d["vector_result"], d)
				}
				val := vr[0]["value"].(map[string]any)
				if val["timestamp"].(float64) != 1700000000 || val["value"] != "1.5" {
					t.Errorf("value = %+v", val)
				}
			},
		},
		{
			name: "matrix",
			body: `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{"pod":"p"},"values":[[1700,"1"],[1760,"2"]]}]}}`,
			assert: func(t *testing.T, d map[string]any) {
				if d["result_type"] != "matrix" {
					t.Errorf("result_type = %v", d["result_type"])
				}
				rs, ok := d["series_list_result"].([]map[string]any)
				if !ok || len(rs) != 1 {
					t.Fatalf("series_list_result missing")
				}
				ts := rs[0]["timestamps"].([]float64)
				vs := rs[0]["values"].([]string)
				if len(ts) != 2 || ts[0] != 1700 || ts[1] != 1760 {
					t.Errorf("timestamps = %v", ts)
				}
				if len(vs) != 2 || vs[0] != "1" || vs[1] != "2" {
					t.Errorf("values = %v", vs)
				}
			},
		},
		{
			name: "scalar",
			body: `{"status":"success","data":{"resultType":"scalar","result":[1700,"42"]}}`,
			assert: func(t *testing.T, d map[string]any) {
				s := d["scalar_result"].(map[string]any)
				if s["timestamp"].(float64) != 1700 || s["value"] != "42" {
					t.Errorf("scalar = %+v", s)
				}
			},
		},
		{
			name: "error",
			body: `{"status":"error","errorType":"bad_data","error":"oh no","data":{"resultType":"","result":null}}`,
			assert: func(t *testing.T, d map[string]any) {
				if d["result_type"] != "error" || d["string_result"] != "oh no" {
					t.Errorf("error result = %+v", d)
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := PrometheusQueryResultDict(json.RawMessage(c.body))
			if err != nil {
				t.Fatalf("PrometheusQueryResultDict: %v", err)
			}
			c.assert(t, out)
		})
	}
}
