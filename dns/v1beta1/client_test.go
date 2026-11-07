package dnsv1beta1_test

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
	dnsv1beta1 "go.origincloud.eu/sdk/dns/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"
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

// DNS is global: a client with a region still calls the global host.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec, core.WithRegion("eu-central1"))
	ctx := context.Background()
	if _, err := dnsv1beta1.NewHostedZoneClient(client).GetHostedZone(ctx, &dnsv1beta1.GetHostedZoneRequest{Name: "projects/p/hostedZones/z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dnsv1beta1.NewRecordSetClient(client).ListRecordSets(ctx, &dnsv1beta1.ListRecordSetsRequest{Parent: "projects/p/hostedZones/z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dnsv1beta1.NewVpcAttachmentClient(client).ListVpcAttachments(ctx, &dnsv1beta1.ListVpcAttachmentsRequest{Parent: "projects/p/hostedZones/z"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://dns.origincloudapis.eu/v1beta1/projects/p/hostedZones/z",
		"https://dns.origincloudapis.eu/v1beta1/projects/p/hostedZones/z/recordSets",
		"https://dns.origincloudapis.eu/v1beta1/projects/p/hostedZones/z/vpcAttachments",
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

// Creating a zone and changing its records are each sent with a request
// id, so a retry after a lost answer isn't a second zone or a second
// change; checking a zone's challenge is sent as its HTTP rule says.
func TestEachZoneCallIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec)
	zones, records := dnsv1beta1.NewHostedZoneClient(client), dnsv1beta1.NewRecordSetClient(client)
	ctx := context.Background()

	if _, err := zones.CreateHostedZone(ctx, &dnsv1beta1.CreateHostedZoneRequest{
		Parent:     "projects/p",
		HostedZone: &dnsv1beta1.HostedZone{DnsName: "example.com", Visibility: dnsv1beta1.HostedZone_PUBLIC},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := records.BatchChangeRecordSets(ctx, &dnsv1beta1.BatchChangeRecordSetsRequest{
		Parent:    "projects/p/hostedZones/z",
		Deletions: []string{"projects/p/hostedZones/z/recordSets/old"},
		Creations: []*dnsv1beta1.RecordSet{{
			DnsName: "www.example.com",
			Type:    dnsv1beta1.RecordSet_A,
			Ttl:     durationpb.New(60_000_000_000),
			Data:    &dnsv1beta1.RecordSet_Basic{Basic: &dnsv1beta1.BasicRecordData{Values: []string{"192.0.2.1"}}},
		}},
		RequestId: "given",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := zones.VerifyHostedZone(ctx, &dnsv1beta1.VerifyHostedZoneRequest{Name: "projects/p/hostedZones/z"}); err != nil {
		t.Fatal(err)
	}

	created, batch, verified := rec.calls[0], rec.calls[1], rec.calls[2]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/projects/p/hostedZones" ||
		!sameJSON(created.body, `{"dnsName":"example.com","visibility":"PUBLIC"}`) ||
		!uuid.MatchString(q.Get("requestId")) || len(q) != 1 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	// body: "*": the whole request is the body, its request id included.
	if batch.method != "POST" || batch.url.Path != "/v1beta1/projects/p/hostedZones/z/recordSets:batchChange" || batch.url.RawQuery != "" ||
		!sameJSON(batch.body, `{"parent":"projects/p/hostedZones/z","deletions":["projects/p/hostedZones/z/recordSets/old"],`+
			`"creations":[{"dnsName":"www.example.com","type":"A","ttl":"60s","basic":{"values":["192.0.2.1"]}}],"requestId":"given"}`) {
		t.Errorf("batch: %s %s %s", batch.method, batch.url, batch.body)
	}
	if verified.method != "POST" || verified.url.Path != "/v1beta1/projects/p/hostedZones/z:verify" || verified.url.RawQuery != "" ||
		!sameJSON(verified.body, `{"name":"projects/p/hostedZones/z"}`) {
		t.Errorf("verify: %s %s %s", verified.method, verified.url, verified.body)
	}
}

func TestAZoneDecodesItsStateAndNameServers(t *testing.T) {
	rec := &recorder{body: `{"name":"projects/p/hostedZones/z","dnsName":"example.com","visibility":"PUBLIC","state":"ACTIVE",` +
		`"nameServers":["ns1.origincloud.eu","ns2.origincloud.eu","ns3.origincloud.eu"],"tags":{"team":"web"}}`}
	got, err := dnsv1beta1.NewHostedZoneClient(newClient(t, rec)).GetHostedZone(context.Background(), &dnsv1beta1.GetHostedZoneRequest{Name: "projects/p/hostedZones/z"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetState() != dnsv1beta1.HostedZone_ACTIVE || got.GetVisibility() != dnsv1beta1.HostedZone_PUBLIC ||
		len(got.GetNameServers()) != 3 || got.GetTags()["team"] != "web" {
		t.Errorf("got %v", got)
	}
}
