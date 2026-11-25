package dispatch

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nudgebee/nudgebee-agent/pkg/auth"
	"github.com/nudgebee/nudgebee-agent/pkg/relaysig"
)

func msgKey(t *testing.T) (ed25519.PrivateKey, *relaysig.Verifier) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 21)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	ver, err := relaysig.NewVerifier(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return priv, ver
}

// signMsg builds what the relay sends: the outer message plus the v2 envelope
// over signedPayload (normally the same bytes as outer).
func signMsg(t *testing.T, priv ed25519.PrivateKey, kind string, outer, signedPayload map[string]any, nonce string) []byte {
	t.Helper()
	sp, _ := json.Marshal(signedPayload)
	at := time.Now().UTC().Format(time.RFC3339)
	m := map[string]any{}
	for k, v := range outer {
		m[k] = v
	}
	m["relay_signed_payload"] = string(sp)
	m["relay_signature_v2"] = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, relaysig.V2Message(kind, at, nonce, sp)))
	m["relay_signed_at"] = at
	m["relay_nonce"] = nonce
	b, _ := json.Marshal(m)
	return b
}

func keyedDispatcher(ver *relaysig.Verifier, signedWrites bool) (*Dispatcher, *stubTerminal, *stubGrafana) {
	v := &auth.Validator{LightActions: map[string]struct{}{}, RelayVerifier: ver, SignedMessageWrites: signedWrites}
	d := New(Config{}, v, map[string]Handler{})
	term, graf := &stubTerminal{}, &stubGrafana{}
	d.SetTerminal(term)
	d.SetGrafana(graf)
	return d, term, graf
}

func send(d *Dispatcher, msg []byte) *captureSend {
	cap := &captureSend{}
	d.Handle(context.Background(), msg, cap.send)
	return cap
}

func TestTerminalAuth(t *testing.T) {
	priv, ver := msgKey(t)
	start := map[string]any{"action": "start", "name": "web", "namespace": "shop", "request_id": "r1"}

	t.Run("unsigned start refused", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, true)
		b, _ := json.Marshal(start)
		r := send(d, b).only(t)
		if r.StatusCode != 401 || len(term.got) != 0 {
			t.Fatalf("status=%d calls=%d; want 401 and no call", r.StatusCode, len(term.got))
		}
		if r.OutputType != "Terminal" || r.RequestID != "r1" {
			t.Errorf("refusal envelope: output_type=%q request_id=%q", r.OutputType, r.RequestID)
		}
	})
	t.Run("unsigned read and close run", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, true)
		for _, a := range []string{"read", "close"} {
			b, _ := json.Marshal(map[string]any{"action": a, "session_id": "s1", "request_id": "r-" + a})
			send(d, b)
		}
		if len(term.got) != 2 {
			t.Fatalf("calls = %d; want 2", len(term.got))
		}
	})
	t.Run("signed start runs the signed request, not the outer one", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, true)
		outer := map[string]any{"action": "start", "name": "other", "namespace": "kube-system", "container": "x", "request_id": "r1"}
		r := send(d, signMsg(t, priv, relaysig.KindTerminal, outer, start, "n-1")).only(t)
		if r.StatusCode != 200 || len(term.got) != 1 {
			t.Fatalf("status=%d calls=%d", r.StatusCode, len(term.got))
		}
		got := term.got[0]
		if got.Name != "web" || got.Namespace != "shop" || got.Container != "" {
			t.Fatalf("ran %+v; want the signed request (web/shop, no container)", got)
		}
	})
	t.Run("replayed signed start is a duplicate", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, true)
		msg := signMsg(t, priv, relaysig.KindTerminal, start, start, "n-2")
		send(d, msg)
		r := send(d, msg).only(t)
		if r.StatusCode != 409 || len(term.got) != 1 {
			t.Fatalf("status=%d calls=%d; want 409 and one call", r.StatusCode, len(term.got))
		}
	})
	t.Run("signature for another kind refused", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, true)
		r := send(d, signMsg(t, priv, relaysig.KindHTTP, start, start, "n-3")).only(t)
		if r.StatusCode != 401 || len(term.got) != 0 {
			t.Fatalf("status=%d calls=%d; want 401", r.StatusCode, len(term.got))
		}
	})
	t.Run("signed writes off: unsigned start runs", func(t *testing.T) {
		d, term, _ := keyedDispatcher(ver, false)
		b, _ := json.Marshal(start)
		send(d, b)
		if len(term.got) != 1 {
			t.Fatalf("calls = %d; want 1", len(term.got))
		}
	})
	t.Run("no key: unsigned start runs", func(t *testing.T) {
		disabled, _ := relaysig.NewVerifier("", slog.New(slog.NewTextHandler(io.Discard, nil)))
		d, term, _ := keyedDispatcher(disabled, false)
		b, _ := json.Marshal(start)
		send(d, b)
		if len(term.got) != 1 {
			t.Fatalf("calls = %d; want 1", len(term.got))
		}
	})
}

func TestGrafanaAuth(t *testing.T) {
	priv, ver := msgKey(t)
	req := func(method, url, typ string) map[string]any {
		h := map[string][]string{"X-Nb-Request-Id": {"g1"}}
		if typ != "" {
			h["X-NB-Request-Type"] = []string{typ}
		}
		return map[string]any{"method": method, "url": url, "header": h}
	}

	t.Run("unsigned reads run, unsigned writes refused", func(t *testing.T) {
		d, _, graf := keyedDispatcher(ver, true)
		for _, m := range []map[string]any{req("GET", "/api/datasources", ""), req("POST", "/api/ds/query", ""), req("POST", "/api/v1/query", "Prometheus")} {
			b, _ := json.Marshal(m)
			if r := send(d, b).only(t); r.StatusCode != 200 {
				t.Errorf("%v: status %d; want 200", m["url"], r.StatusCode)
			}
		}
		for _, m := range []map[string]any{req("POST", "/api/dashboards/db", ""), req("DELETE", "/api/datasources/1", ""), req("POST", "/v1/data", "APIProxy"), req("POST", "/api/v1/admin/tsdb/delete_series", "Prometheus")} {
			b, _ := json.Marshal(m)
			r := send(d, b).only(t)
			if r.StatusCode != 401 || r.OutputType != "Grafana" || r.RequestID != "g1" {
				t.Errorf("%v: status=%d output_type=%q request_id=%q; want 401 Grafana g1", m["url"], r.StatusCode, r.OutputType, r.RequestID)
			}
		}
		if len(graf.gotG)+len(graf.gotProm) != 3 || len(graf.gotAPI) != 0 {
			t.Fatalf("calls: grafana=%d prom=%d api=%d", len(graf.gotG), len(graf.gotProm), len(graf.gotAPI))
		}
	})
	t.Run("signed write runs the signed request", func(t *testing.T) {
		d, _, graf := keyedDispatcher(ver, true)
		signed := req("POST", "/v1/data", "APIProxy")
		signed["header"].(map[string][]string)["X-API-Base-URL"] = []string{"https://api.example.com"}
		outer := req("POST", "/v1/other", "APIProxy")
		outer["header"].(map[string][]string)["X-API-Base-URL"] = []string{"https://evil.example.net"}
		r := send(d, signMsg(t, priv, relaysig.KindHTTP, outer, signed, "g-1")).only(t)
		if r.StatusCode != 200 || len(graf.gotAPI) != 1 {
			t.Fatalf("status=%d api calls=%d", r.StatusCode, len(graf.gotAPI))
		}
		if graf.gotAPI[0].base != "https://api.example.com" || graf.gotAPI[0].req.URL != "/v1/data" {
			t.Fatalf("ran base=%q url=%q; want the signed ones", graf.gotAPI[0].base, graf.gotAPI[0].req.URL)
		}
	})
}

// Two spellings of the request-type header could make the read/write check
// and the proxy pick different types; such a message is refused.
func TestGrafanaAuth_AmbiguousRoutingHeader(t *testing.T) {
	_, ver := msgKey(t)
	d, _, graf := keyedDispatcher(ver, true)
	b, _ := json.Marshal(map[string]any{
		"method": "POST", "url": "/api/v1/query",
		"header": map[string][]string{
			"X-Nb-Request-Id":   {"g1"},
			"X-NB-Request-Type": {"Prometheus"},
			"x-nb-request-type": {"APIProxy"},
			"X-API-Base-URL":    {"https://api.example.com"},
		},
	})
	r := send(d, b).only(t)
	if r.StatusCode != 400 || len(graf.gotAPI)+len(graf.gotProm) != 0 {
		t.Fatalf("status=%d api=%d prom=%d; want 400 and no call", r.StatusCode, len(graf.gotAPI), len(graf.gotProm))
	}
}

// A verified message whose signed request lacks the request id still gets its
// reply, under the outer id.
func TestTerminalAuth_SignedWithoutRequestIDUsesOuter(t *testing.T) {
	priv, ver := msgKey(t)
	d, term, _ := keyedDispatcher(ver, true)
	signed := map[string]any{"action": "start", "name": "web", "namespace": "shop"}
	outer := map[string]any{"action": "start", "name": "web", "namespace": "shop", "request_id": "outer-1"}
	r := send(d, signMsg(t, priv, relaysig.KindTerminal, outer, signed, "n-id")).only(t)
	if r.RequestID != "outer-1" || len(term.got) != 1 {
		t.Fatalf("request_id=%q calls=%d", r.RequestID, len(term.got))
	}
}
