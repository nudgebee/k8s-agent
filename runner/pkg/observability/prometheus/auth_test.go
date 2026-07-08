package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCoralogixAuth_SetsTokenHeader(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("token")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Auth = NewCoralogixAuth("cx-token")
	if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
		t.Fatal(err)
	}
	if got != "cx-token" {
		t.Errorf("token header = %q", got)
	}
}

func TestAWSAuth_SignsRequest(t *testing.T) {
	var auth, amzDate string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		amzDate = r.Header.Get("X-Amz-Date")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Auth = NewAWSAuth("AKIDEXAMPLE", "secret", "us-east-1", "aps")
	if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
		t.Fatal(err)
	}
	// SigV4 stamps an Authorization header with the algorithm + credential
	// scope (service "aps", region "us-east-1") and an X-Amz-Date.
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("Authorization not SigV4: %q", auth)
	}
	if !strings.Contains(auth, "/us-east-1/aps/aws4_request") {
		t.Errorf("credential scope missing service/region: %q", auth)
	}
	if amzDate == "" {
		t.Error("X-Amz-Date not set")
	}
}

func TestAWSAuth_DefaultsServiceToAPS(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Auth = NewAWSAuth("AKIDEXAMPLE", "secret", "eu-west-1", "")
	if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(auth, "/eu-west-1/aps/aws4_request") {
		t.Errorf("expected default service aps: %q", auth)
	}
}

func TestAzureAuth_MintsAndCachesBearer(t *testing.T) {
	var tokenCalls int
	var seenBearer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/token") {
			tokenCalls++
			_, _ = w.Write([]byte(`{"access_token":"az-tok","expires_in":"3600"}`))
			return
		}
		seenBearer = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, &http.Client{Timeout: 5 * time.Second})
	c.Auth = NewAzureAuth(AzureAuthConfig{
		ClientID:      "cid",
		ClientSecret:  "csecret",
		TenantID:      "tid",
		Resource:      "https://prometheus.monitor.azure.com",
		TokenEndpoint: srv.URL + "/tenant/oauth2/token",
	}, &http.Client{Timeout: 5 * time.Second})

	for i := 0; i < 3; i++ {
		if _, err := c.Query(context.Background(), "up", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if seenBearer != "Bearer az-tok" {
		t.Errorf("Authorization = %q", seenBearer)
	}
	if tokenCalls != 1 {
		t.Errorf("expected token cached (1 mint), got %d", tokenCalls)
	}
}

func TestNewAzureAuth_NilWhenIncomplete(t *testing.T) {
	// Missing tenant + no managed id / secret → not authorized.
	if a := NewAzureAuth(AzureAuthConfig{ClientID: "cid"}, nil); a != nil {
		t.Error("expected nil authorizer for incomplete Azure config")
	}
}

func TestAzureExpiry_PrefersExpiresOn(t *testing.T) {
	got := azureExpiry(azureTokenResponse{ExpiresOn: "1700000000"})
	if got.Unix() != 1700000000 {
		t.Errorf("expiry = %d", got.Unix())
	}
	// Falls back to now+expires_in when expires_on absent.
	got = azureExpiry(azureTokenResponse{ExpiresIn: "120"})
	if d := time.Until(got); d < 100*time.Second || d > 140*time.Second {
		t.Errorf("expires_in fallback out of range: %v", d)
	}
}
