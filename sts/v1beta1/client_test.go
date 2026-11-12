package stsv1beta1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.origincloud.eu/sdk/core"
	stsv1beta1 "go.origincloud.eu/sdk/sts/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"
)

// recorder answers every request with body and records what it was sent.
type recorder struct {
	body     string
	urls     []string
	requests []map[string]any
	auth     []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.Method+" "+req.URL.String())
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	var sent map[string]any
	if req.Body != nil {
		_ = json.NewDecoder(req.Body).Decode(&sent)
	}
	r.requests = append(r.requests, sent)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func newClient(t *testing.T, rec *recorder, opts ...core.Option) *stsv1beta1.SecurityTokenClient {
	t.Helper()
	opts = append(opts, core.WithCredentials(core.AccessToken("t")), core.WithHTTPClient(&http.Client{Transport: rec}))
	client, err := core.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return stsv1beta1.NewSecurityTokenClient(client)
}

// The STS answers at its global host only: a client with a region still
// calls it there. It authenticates no caller: the client's credentials
// aren't sent.
func TestAssumeServiceAccountCallsTheGlobalHostWithTheToken(t *testing.T) {
	rec := &recorder{body: `{
		"accessToken": "issued",
		"expireTime": "2026-11-11T20:00:00Z",
		"serviceAccount": "organizations/o/serviceAccounts/s",
		"federatedCredential": "organizations/o/serviceAccounts/s/federatedCredentials/f"
	}`}
	resp, err := newClient(t, rec, core.WithRegion("eu-central1")).AssumeServiceAccount(context.Background(), &stsv1beta1.AssumeServiceAccountRequest{
		OidcToken:      "job-token",
		ServiceAccount: "organizations/o/serviceAccounts/s",
		Lifetime:       durationpb.New(10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := "POST https://sts.origincloudapis.eu/v1beta1:assumeServiceAccount"; len(rec.urls) != 1 || rec.urls[0] != want {
		t.Fatalf("called %v, want [%s]", rec.urls, want)
	}
	if rec.auth[0] != "" {
		t.Fatalf("sent Authorization %q, want none", rec.auth[0])
	}
	sent := rec.requests[0]
	if sent["oidcToken"] != "job-token" || sent["serviceAccount"] != "organizations/o/serviceAccounts/s" || sent["lifetime"] != "600s" {
		t.Fatalf("sent %v", sent)
	}
	if resp.GetAccessToken() != "issued" ||
		resp.GetServiceAccount() != "organizations/o/serviceAccounts/s" ||
		resp.GetFederatedCredential() != "organizations/o/serviceAccounts/s/federatedCredentials/f" ||
		!resp.GetExpireTime().AsTime().Equal(time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v", resp)
	}
}
