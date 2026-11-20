package alerts

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// changeEngine records what it was asked to match and returns one match.
type changeEngine struct {
	calls chan [2]string // operation, kind
}

func (e *changeEngine) MatchK8sEvent(op, kind string, obj, _ map[string]any) []MatchedTrigger {
	e.calls <- [2]string{op, kind}
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	return []MatchedTrigger{{
		AggregationKey: "report_crash_loop", Priority: "HIGH", FindingType: "issue",
		SubjectName: name, SubjectNamespace: "prod", SubjectKind: "pod", MatcherName: "pod_crash_loop",
	}}
}

const podChange = `{"operation":"update","kind":"Pod","clusterUid":"","description":"d","apiVersion":"v1",` +
	`"obj":{"metadata":{"name":"web-0","namespace":"prod"}},"oldObj":{"metadata":{"name":"web-0","namespace":"prod"}}}`

// A change from the runner's own watch goes through the same matching and
// forwarding as a kubewatch post, and its payload becomes the json evidence.
func TestForwarder_HandleK8sChangeForwardsLikeKubewatch(t *testing.T) {
	got := make(chan []byte, 2)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	f := NewForwarder(backend.URL, "", "acct", "cluster", slog.Default())
	eng := &changeEngine{calls: make(chan [2]string, 1)}
	f.Engine = eng

	f.HandleK8sChange("update", "Pod", json.RawMessage(podChange))

	if c := <-eng.calls; c != [2]string{"update", "Pod"} {
		t.Errorf("engine got %v", c)
	}
	select {
	case body := <-got:
		var env FindingEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatal(err)
		}
		if env.Finding.AggregationKey != "report_crash_loop" || env.Finding.SubjectName != "web-0" {
			t.Errorf("finding = %+v", env.Finding)
		}
		if len(env.Evidence) == 0 || !strings.Contains(env.Evidence[0].Data, `\"description\":\"d\"`) {
			t.Errorf("evidence does not carry the change payload: %+v", env.Evidence)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no Finding forwarded")
	}
}

// While the runner watches Kubernetes itself, kubewatch's posts are accepted
// and ignored, so one change cannot become two Findings.
func TestForwarder_KubewatchIgnoredWhenSuperseded(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("nothing should be forwarded")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	f := NewForwarder(backend.URL, "", "acct", "cluster", slog.Default())
	eng := &changeEngine{calls: make(chan [2]string, 1)}
	f.Engine = eng
	f.KubewatchSuperseded = true
	srv := httptest.NewServer(f.Mux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/api/handle", "application/json", strings.NewReader(`{"data":`+podChange+`}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d; want 202 so kubewatch does not retry", resp.StatusCode)
	}
	select {
	case c := <-eng.calls:
		t.Errorf("engine ran on a superseded kubewatch post: %v", c)
	case <-time.After(300 * time.Millisecond):
	}
}
