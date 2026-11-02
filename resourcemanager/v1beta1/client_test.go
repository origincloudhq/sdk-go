package resourcemanagerv1beta1_test

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

	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"go.origincloud.eu/sdk/core"
	resourcemanagerv1beta1 "go.origincloud.eu/sdk/resourcemanager/v1beta1"
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

// Organizations and projects are global: a client with a region still
// calls the global host.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec, core.WithRegion("eu-central1"))
	ctx := context.Background()
	if _, err := resourcemanagerv1beta1.NewProjectClient(client).GetProject(ctx, &resourcemanagerv1beta1.GetProjectRequest{Name: "projects/p"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resourcemanagerv1beta1.NewOrganizationClient(client).GetOrganization(ctx, &resourcemanagerv1beta1.GetOrganizationRequest{Name: "organizations/o"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://resourcemanager.origincloudapis.eu/v1beta1/projects/p",
		"https://resourcemanager.origincloudapis.eu/v1beta1/organizations/o",
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

func TestEachProjectBindingIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{body: `{}`}
	projects := resourcemanagerv1beta1.NewProjectClient(newClient(t, rec))
	ctx := context.Background()

	if _, err := projects.CreateProject(ctx, &resourcemanagerv1beta1.CreateProjectRequest{
		Project: &resourcemanagerv1beta1.Project{Organization: "organizations/o", DisplayName: "Production"}, ProjectId: "acme-prod",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := projects.UpdateProject(ctx, &resourcemanagerv1beta1.UpdateProjectRequest{
		Project:    &resourcemanagerv1beta1.Project{Name: "projects/acme-prod", DisplayName: "Prod"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := projects.ListProjects(ctx, &resourcemanagerv1beta1.ListProjectsRequest{Organization: "organizations/o", PageSize: 5, PageToken: "next"}); err != nil {
		t.Fatal(err)
	}

	created, updated, listed := rec.calls[0], rec.calls[1], rec.calls[2]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/projects" ||
		!sameJSON(created.body, `{"organization":"organizations/o","displayName":"Production"}`) ||
		q.Get("projectId") != "acme-prod" || !uuid.MatchString(q.Get("requestId")) || len(q) != 2 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	if q := updated.url.Query(); updated.method != "PATCH" || updated.url.Path != "/v1beta1/projects/acme-prod" ||
		!sameJSON(updated.body, `{"name":"projects/acme-prod","displayName":"Prod"}`) ||
		q.Get("updateMask") != "displayName" || len(q) != 1 {
		t.Errorf("update: %s %s %s", updated.method, updated.url, updated.body)
	}
	if q := listed.url.Query(); listed.method != "GET" || listed.url.Path != "/v1beta1/projects" || listed.body != "" ||
		q.Get("organization") != "organizations/o" || q.Get("pageSize") != "5" || q.Get("pageToken") != "next" || len(q) != 3 {
		t.Errorf("list: %s %s %s", listed.method, listed.url, listed.body)
	}
}

// An unset budget means none, which differs from a budget of zero.
func TestAProjectQuotaTellsAnUnsetBudgetFromZero(t *testing.T) {
	rec := &recorder{body: `{"name":"projects/p/quota","maxInstances":"0","maxVcpu":"16"}`}
	projects := resourcemanagerv1beta1.NewProjectClient(newClient(t, rec))

	quota, err := projects.GetProjectQuota(context.Background(), &resourcemanagerv1beta1.GetProjectQuotaRequest{Name: "projects/p/quota"})
	if err != nil {
		t.Fatal(err)
	}
	if quota.MaxInstances == nil || quota.GetMaxInstances() != 0 || quota.GetMaxVcpu() != 16 || quota.MaxVolumeGib != nil {
		t.Errorf("got %v", quota)
	}
	if got := rec.calls[0]; got.method != "GET" || got.url.Path != "/v1beta1/projects/p/quota" {
		t.Errorf("get: %s %s", got.method, got.url)
	}
}
