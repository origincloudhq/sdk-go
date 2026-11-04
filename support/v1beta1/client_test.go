package supportv1beta1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"go.origincloud.eu/sdk/core"
	supportv1beta1 "go.origincloud.eu/sdk/support/v1beta1"
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

// Support is global: a client with a region still calls the global host.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec, core.WithRegion("eu-central1"))
	ctx := context.Background()
	if _, err := supportv1beta1.NewCaseClient(client).GetCase(ctx, &supportv1beta1.GetCaseRequest{Name: "organizations/o/cases/c"}); err != nil {
		t.Fatal(err)
	}
	if _, err := supportv1beta1.NewCaseMessageClient(client).ListCaseMessages(ctx, &supportv1beta1.ListCaseMessagesRequest{Parent: "organizations/o/cases/c"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://support.origincloudapis.eu/v1beta1/organizations/o/cases/c",
		"https://support.origincloudapis.eu/v1beta1/organizations/o/cases/c/messages",
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

// Opening a case and writing in it are each sent with a request id, so a
// retry after a lost answer isn't a second case or a second message.
func TestEachCaseCallIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec)
	cases, messages := supportv1beta1.NewCaseClient(client), supportv1beta1.NewCaseMessageClient(client)
	ctx := context.Background()

	if _, err := cases.CreateCase(ctx, &supportv1beta1.CreateCaseRequest{
		Parent: "organizations/o",
		Case:   &supportv1beta1.Case{Subject: "Volume stuck", Description: "It stays ATTACHING.", ContactEmail: "ada@example.com", Project: "projects/p"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := messages.CreateCaseMessage(ctx, &supportv1beta1.CreateCaseMessageRequest{
		Parent: "organizations/o/cases/c", CaseMessage: &supportv1beta1.CaseMessage{Body: "Still stuck."}, RequestId: "given",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := cases.CloseCase(ctx, &supportv1beta1.CloseCaseRequest{Name: "organizations/o/cases/c"}); err != nil {
		t.Fatal(err)
	}

	created, wrote, closed := rec.calls[0], rec.calls[1], rec.calls[2]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/organizations/o/cases" ||
		!sameJSON(created.body, `{"subject":"Volume stuck","description":"It stays ATTACHING.","contactEmail":"ada@example.com","project":"projects/p"}`) ||
		!uuid.MatchString(q.Get("requestId")) || len(q) != 1 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	if q := wrote.url.Query(); wrote.method != "POST" || wrote.url.Path != "/v1beta1/organizations/o/cases/c/messages" ||
		!sameJSON(wrote.body, `{"body":"Still stuck."}`) || q.Get("requestId") != "given" || len(q) != 1 {
		t.Errorf("message: %s %s %s", wrote.method, wrote.url, wrote.body)
	}
	// body: "*": the whole request is the body.
	if closed.method != "POST" || closed.url.Path != "/v1beta1/organizations/o/cases/c:close" || closed.url.RawQuery != "" ||
		!sameJSON(closed.body, `{"name":"organizations/o/cases/c"}`) {
		t.Errorf("close: %s %s %s", closed.method, closed.url, closed.body)
	}
}

func TestACaseDecodesItsState(t *testing.T) {
	rec := &recorder{body: `{"name":"organizations/o/cases/c","subject":"Volume stuck","state":"IN_PROGRESS",` +
		`"createTime":"2026-11-03T09:00:00Z","updateTime":"2026-11-03T10:30:00Z"}`}
	got, err := supportv1beta1.NewCaseClient(newClient(t, rec)).GetCase(context.Background(), &supportv1beta1.GetCaseRequest{Name: "organizations/o/cases/c"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != supportv1beta1.Case_IN_PROGRESS || got.GetSubject() != "Volume stuck" || got.GetProject() != "" ||
		got.GetUpdateTime().AsTime().Sub(got.GetCreateTime().AsTime()).Minutes() != 90 {
		t.Errorf("got %v", got)
	}
}
