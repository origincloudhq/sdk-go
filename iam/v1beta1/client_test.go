package iamv1beta1_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.origincloud.eu/sdk/core"
	iamv1beta1 "go.origincloud.eu/sdk/iam/v1beta1"
)

// A request as it was sent.
type call struct {
	method, body string
	url          *url.URL
}

// recorder answers every request with body and records the requests it
// was sent.
type recorder struct {
	body  string
	calls []call
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.calls = append(r.calls, call{req.Method, string(body), req.URL})
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func newClient(t *testing.T, rec *recorder, opts ...core.Option) *core.Client {
	t.Helper()
	opts = append(opts, core.WithCredentials(core.AccessToken("t")), core.WithHTTPClient(&http.Client{Transport: rec}))
	client, err := core.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

var uuid = regexp.MustCompile(`^[0-9a-f-]{36}$`)

// sameJSON compares JSON by value: protojson varies its whitespace on
// purpose.
func sameJSON(got, want string) bool {
	var g, w any
	return json.Unmarshal([]byte(got), &g) == nil && json.Unmarshal([]byte(want), &w) == nil && reflect.DeepEqual(g, w)
}

// IAM is global: a client with a region still calls the global host.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec, core.WithRegion("eu-central1"))
	ctx := context.Background()
	if _, err := iamv1beta1.NewServiceAccountClient(client).GetServiceAccount(ctx, &iamv1beta1.GetServiceAccountRequest{Name: "organizations/o/serviceAccounts/s"}); err != nil {
		t.Fatal(err)
	}
	if _, err := iamv1beta1.NewUserClient(client).GetUser(ctx, &iamv1beta1.GetUserRequest{Name: "users/me"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://iam.origincloudapis.eu/v1beta1/organizations/o/serviceAccounts/s",
		"https://iam.origincloudapis.eu/v1beta1/users/me",
	}
	if len(rec.calls) != len(want) {
		t.Fatalf("sent %d calls, want %d", len(rec.calls), len(want))
	}
	for i, c := range rec.calls {
		if c.url.String() != want[i] {
			t.Errorf("called %s, want %s", c.url, want[i])
		}
	}
}

func TestEachRoleBindingBindingIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{body: `{}`}
	bindings := iamv1beta1.NewRoleBindingClient(newClient(t, rec))
	ctx := context.Background()

	if _, err := bindings.CreateRoleBinding(ctx, &iamv1beta1.CreateRoleBindingRequest{
		Parent:      "organizations/o",
		RoleBinding: &iamv1beta1.RoleBinding{Member: "user:ada@example.com", Role: "roles/viewer", Project: "projects/p"},
		RequestId:   "given",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := bindings.ListRoleBindings(ctx, &iamv1beta1.ListRoleBindingsRequest{Parent: "organizations/o", Member: "user:ada@example.com", PageSize: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := bindings.DeleteRoleBinding(ctx, &iamv1beta1.DeleteRoleBindingRequest{Name: "organizations/o/roleBindings/b"}); err != nil {
		t.Fatal(err)
	}

	created, listed, deleted := rec.calls[0], rec.calls[1], rec.calls[2]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/organizations/o/roleBindings" ||
		!sameJSON(created.body, `{"member":"user:ada@example.com","role":"roles/viewer","project":"projects/p"}`) ||
		q.Get("requestId") != "given" || len(q) != 1 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	if q := listed.url.Query(); listed.method != "GET" || listed.url.Path != "/v1beta1/organizations/o/roleBindings" || listed.body != "" ||
		q.Get("member") != "user:ada@example.com" || q.Get("pageSize") != "5" || len(q) != 2 {
		t.Errorf("list: %s %s %s", listed.method, listed.url, listed.body)
	}
	if q := deleted.url.Query(); deleted.method != "DELETE" || deleted.url.Path != "/v1beta1/organizations/o/roleBindings/b" || deleted.body != "" ||
		!uuid.MatchString(q.Get("requestId")) || len(q) != 1 {
		t.Errorf("delete: %s %s %s", deleted.method, deleted.url, deleted.body)
	}
}

// The credentials file is shown once, in the response that created the
// key.
func TestCreateServiceAccountKeyAnswersWithItsCredentialsFile(t *testing.T) {
	rec := &recorder{body: `{"name":"organizations/o/serviceAccounts/s/keys/k","algorithm":"ES256","state":"ACTIVE",` +
		`"credentialsFile":"{\"type\":\"service_account_key\"}"}`}
	keys := iamv1beta1.NewServiceAccountKeyClient(newClient(t, rec))

	key, err := keys.CreateServiceAccountKey(context.Background(), &iamv1beta1.CreateServiceAccountKeyRequest{
		Parent: "organizations/o/serviceAccounts/s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if key.GetCredentialsFile() != `{"type":"service_account_key"}` || key.GetName() != "organizations/o/serviceAccounts/s/keys/k" {
		t.Errorf("got %v", key)
	}
	if got := rec.calls[0]; got.method != "POST" || got.url.Path != "/v1beta1/organizations/o/serviceAccounts/s/keys" ||
		!uuid.MatchString(got.url.Query().Get("requestId")) || len(got.url.Query()) != 1 {
		t.Errorf("create: %s %s %s", got.method, got.url, got.body)
	}
}

// lostAnswer records the requests it was sent and loses the answer to the
// first, as a connection that breaks off after the server acted on it
// does, then answers every later one with status and body.
type lostAnswer struct {
	status int
	body   string
	calls  []call
}

func (l *lostAnswer) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	l.calls = append(l.calls, call{req.Method, string(body), req.URL})
	if len(l.calls) == 1 {
		return nil, io.ErrUnexpectedEOF
	}
	return &http.Response{
		StatusCode: l.status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(l.body)),
		Request:    req,
	}, nil
}

// A create whose answer was lost is sent again with the same request id.
// The server already created the key then, and its credentials file went
// out with the lost answer: the repeat is refused with CONFLICT, naming
// the key, and not sent a third time.
func TestARepeatedCreateServiceAccountKeyIsAConflictNamingTheKey(t *testing.T) {
	transport := &lostAnswer{status: http.StatusConflict, body: `{"error":{"code":409,"status":"FAILED_PRECONDITION",` +
		`"message":"the service account key organizations/o/serviceAccounts/s/keys/k was created by an earlier request with this request_id; ` +
		`its credentials file was returned only to that request. Delete that key and create a new one",` +
		`"details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"CONFLICT","domain":"origincloudapis.eu",` +
		`"metadata":{"serviceAccountKey":"organizations/o/serviceAccounts/s/keys/k"}},` +
		`{"@type":"type.googleapis.com/google.rpc.RequestInfo","requestId":"7d3c1a2b9e8f4051"}]}}`}
	client, err := core.NewClient(context.Background(), core.WithCredentials(core.AccessToken("t")), core.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}

	key, err := iamv1beta1.NewServiceAccountKeyClient(client).CreateServiceAccountKey(context.Background(), &iamv1beta1.CreateServiceAccountKeyRequest{
		Parent: "organizations/o/serviceAccounts/s",
	})
	var apiErr *core.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("CreateServiceAccountKey = %v, %v; want a *core.Error", key, err)
	}
	if apiErr.HTTPStatus != http.StatusConflict || apiErr.Reason != "CONFLICT" ||
		apiErr.Metadata["serviceAccountKey"] != "organizations/o/serviceAccounts/s/keys/k" {
		t.Errorf("error = %+v", apiErr)
	}
	if len(transport.calls) != 2 {
		t.Fatalf("sent %d calls, want 2", len(transport.calls))
	}
	first, repeat := transport.calls[0].url.Query().Get("requestId"), transport.calls[1].url.Query().Get("requestId")
	if !uuid.MatchString(first) || repeat != first {
		t.Errorf("request ids %q then %q, want the same one twice", first, repeat)
	}
}
