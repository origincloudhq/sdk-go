package networkv1beta1_test

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
	networkv1beta1 "go.origincloud.eu/sdk/network/v1beta1"
)

// VPCs and security groups live in a region; the operations their changes
// answer with are named in a zone, like every service's.
const (
	region = "projects/p/regions/eu-central1"
	op     = "projects/p/zones/eu-central1-az1/operations/op-1"
)

// A request as it was sent.
type call struct {
	method, body string
	url          *url.URL
}

// recorder records the requests it is sent and answers each with what
// answer returns for it, or an empty JSON object without one.
type recorder struct {
	answer func(*http.Request) string
	calls  []call
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	r.calls = append(r.calls, call{req.Method, string(body), req.URL})
	answer := `{}`
	if r.answer != nil {
		answer = r.answer(req)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(answer)),
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

func TestARegionalClientCallsTheRegionsFrontDoor(t *testing.T) {
	for clientRegion, host := range map[string]string{
		"":            "network.origincloudapis.eu",
		"eu-central1": "network.eu-central1.origincloudapis.eu",
	} {
		rec := &recorder{}
		vpcs := networkv1beta1.NewVpcClient(newClient(t, rec, core.WithRegion(clientRegion)))
		if _, err := vpcs.GetVpc(context.Background(), &networkv1beta1.GetVpcRequest{Name: region + "/vpcs/vpc-1"}); err != nil {
			t.Fatal(err)
		}
		if want := "https://" + host + "/v1beta1/" + region + "/vpcs/vpc-1"; rec.calls[0].url.String() != want {
			t.Errorf("region %q: called %s, want %s", clientRegion, rec.calls[0].url, want)
		}
	}
}

func TestEachVpcBindingIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{}
	vpcs := networkv1beta1.NewVpcClient(newClient(t, rec))
	ctx := context.Background()
	name := region + "/vpcs/vpc-1"

	if _, err := vpcs.CreateVpc(ctx, &networkv1beta1.CreateVpcRequest{
		Parent: region, Vpc: &networkv1beta1.Vpc{Ipv4CidrBlock: "10.0.0.0/16"}, RequestId: "given",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := vpcs.UpdateVpc(ctx, &networkv1beta1.UpdateVpcRequest{
		Vpc:        &networkv1beta1.Vpc{Name: name, Tags: map[string]string{"env": "prod"}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"tags"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := vpcs.ListVpcs(ctx, &networkv1beta1.ListVpcsRequest{Parent: region, PageSize: 5, PageToken: "next"}); err != nil {
		t.Fatal(err)
	}

	created, updated, listed := rec.calls[0], rec.calls[1], rec.calls[2]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/"+region+"/vpcs" ||
		!sameJSON(created.body, `{"ipv4CidrBlock":"10.0.0.0/16"}`) || q.Get("requestId") != "given" || len(q) != 1 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	if q := updated.url.Query(); updated.method != "PATCH" || updated.url.Path != "/v1beta1/"+name ||
		!sameJSON(updated.body, `{"name":"`+name+`","tags":{"env":"prod"}}`) ||
		q.Get("updateMask") != "tags" || !uuid.MatchString(q.Get("requestId")) {
		t.Errorf("update: %s %s %s", updated.method, updated.url, updated.body)
	}
	if q := listed.url.Query(); listed.method != "GET" || listed.url.Path != "/v1beta1/"+region+"/vpcs" || listed.body != "" ||
		q.Get("pageSize") != "5" || q.Get("pageToken") != "next" || len(q) != 2 {
		t.Errorf("list: %s %s %s", listed.method, listed.url, listed.body)
	}
}

func TestAZonalNameIsRefusedForARegionalResourceBeforeSending(t *testing.T) {
	rec := &recorder{}
	vpcs := networkv1beta1.NewVpcClient(newClient(t, rec))

	_, err := vpcs.GetVpc(context.Background(), &networkv1beta1.GetVpcRequest{Name: "projects/p/zones/eu-central1-az1/vpcs/vpc-1"})
	if err == nil || !strings.Contains(err.Error(), "projects/*/regions/*/vpcs/*") {
		t.Fatalf("err = %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("sent %d calls", len(rec.calls))
	}
}

// A rule that admits traffic from another security group names it in
// peerSecurityGroup, on the way out and back.
func TestASecurityGroupRulesPeerRoundTripsThroughItsOperation(t *testing.T) {
	group := region + "/securityGroups/sg-1"
	peer := region + "/securityGroups/sg-2"
	rec := &recorder{answer: func(r *http.Request) string {
		if strings.HasSuffix(r.URL.Path, ":wait") {
			return `{"name":"` + op + `","done":true,"response":{"@type":"type.googleapis.com/origincloud.network.v1beta1.SecurityGroupRule",` +
				`"name":"` + group + `/rules/r-1","direction":"INGRESS","protocol":"TCP","firstPort":22,"lastPort":22,"peerSecurityGroup":"` + peer + `"}}`
		}
		return `{"name":"` + op + `"}`
	}}
	rules := networkv1beta1.NewSecurityGroupRuleClient(newClient(t, rec, core.WithRegion("eu-central1")))

	created, err := rules.CreateSecurityGroupRule(context.Background(), &networkv1beta1.CreateSecurityGroupRuleRequest{
		Parent: group,
		SecurityGroupRule: &networkv1beta1.SecurityGroupRule{
			Direction: networkv1beta1.SecurityGroupRule_INGRESS, Protocol: networkv1beta1.SecurityGroupRule_TCP,
			FirstPort: 22, LastPort: 22, PeerSecurityGroup: peer,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := created.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	sent := rec.calls[0]
	if sent.method != "POST" || sent.url.Path != "/v1beta1/"+group+"/rules" ||
		!sameJSON(sent.body, `{"direction":"INGRESS","protocol":"TCP","firstPort":22,"lastPort":22,"peerSecurityGroup":"`+peer+`"}`) {
		t.Errorf("create: %s %s %s", sent.method, sent.url, sent.body)
	}
	if want := "https://network.eu-central1.origincloudapis.eu/v1beta1/" + op + ":wait"; rec.calls[1].url.String() != want {
		t.Errorf("waited at %s, want %s", rec.calls[1].url, want)
	}
	if rule.GetPeerSecurityGroup() != peer || rule.GetName() != group+"/rules/r-1" {
		t.Errorf("waited for %v", rule)
	}
}
