package secretmanagerv1beta1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.origincloud.eu/sdk/core"
	secretmanagerv1beta1 "go.origincloud.eu/sdk/secretmanager/v1beta1"
)

const (
	region = "projects/shop-123456/regions/eu-central1"
	secret = region + "/secrets/db-password"
)

// fakeSecretManager answers the three calls of the round trip the way the
// API describes them: a secret without a version, a version numbered 1, and
// `latest` resolved to the number of the newest version with its payload.
func fakeSecretManager(t *testing.T, calls *[]string) *httptest.Server {
	t.Helper()
	var payload []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls = append(*calls, r.Method+" "+r.URL.EscapedPath())
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta1/"+region+"/secrets":
			if r.URL.Query().Get("secretId") != "db-password" {
				t.Errorf("create: query %v, want secretId=db-password", r.URL.Query())
			}
			_, _ = io.WriteString(w, `{"name":"`+secret+`"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1beta1/"+secret+":addVersion":
			var req struct{ Payload struct{ Data []byte } }
			if err := json.Unmarshal(body, &req); err != nil {
				t.Errorf("addVersion: body %s: %v", body, err)
			}
			payload = req.Payload.Data
			_, _ = io.WriteString(w, `{"name":"`+secret+`/versions/1","state":"ENABLED"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1beta1/"+secret+"/versions/latest:access":
			out, _ := json.Marshal(map[string]any{
				"secretVersion": "//secretmanager.origincloudapis.eu/" + secret + "/versions/1",
				"payload":       map[string]any{"data": payload},
			})
			_, _ = w.Write(out)
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func TestASecretIsCreatedGivenAVersionAndReadAsLatest(t *testing.T) {
	var calls []string
	server := fakeSecretManager(t, &calls)
	client, err := core.NewClient(context.Background(), core.WithEndpoint(server.URL), core.WithCredentials(core.AccessToken("t")))
	if err != nil {
		t.Fatal(err)
	}
	secrets := secretmanagerv1beta1.NewSecretManagerClient(client)
	ctx := context.Background()

	created, err := secrets.CreateSecret(ctx, &secretmanagerv1beta1.CreateSecretRequest{
		Parent: region, SecretId: "db-password", Secret: &secretmanagerv1beta1.Secret{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.GetName() != secret {
		t.Errorf("created %q, want %q", created.GetName(), secret)
	}

	version, err := secrets.AddSecretVersion(ctx, &secretmanagerv1beta1.AddSecretVersionRequest{
		Parent: created.GetName(), Payload: &secretmanagerv1beta1.SecretPayload{Data: []byte("s3cret")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(version.GetName(), "/versions/1") {
		t.Errorf("added version %q, want number 1", version.GetName())
	}

	got, err := secrets.AccessSecretVersion(ctx, &secretmanagerv1beta1.AccessSecretVersionRequest{Name: created.GetName() + "/versions/latest"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.GetPayload().GetData()) != "s3cret" || !strings.HasSuffix(got.GetSecretVersion(), "/versions/1") {
		t.Errorf("latest = %q at %q, want s3cret at version 1", got.GetPayload().GetData(), got.GetSecretVersion())
	}
	if len(calls) != 3 {
		t.Errorf("calls = %q, want three", calls)
	}
}
