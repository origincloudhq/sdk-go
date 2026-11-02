package computev1beta1_test

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

	computev1beta1 "go.origincloud.eu/sdk/compute/v1beta1"
	"go.origincloud.eu/sdk/core"
)

const zone = "projects/p/zones/eu-central1-az1"

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
	for region, host := range map[string]string{
		"":            "compute.origincloudapis.eu",
		"eu-central1": "compute.eu-central1.origincloudapis.eu",
	} {
		rec := &recorder{}
		instances := computev1beta1.NewInstanceClient(newClient(t, rec, core.WithRegion(region)))
		if _, err := instances.GetInstance(context.Background(), &computev1beta1.GetInstanceRequest{Name: zone + "/instances/i-1"}); err != nil {
			t.Fatal(err)
		}
		if want := "https://" + host + "/v1beta1/" + zone + "/instances/i-1"; rec.calls[0].url.String() != want {
			t.Errorf("region %q: called %s, want %s", region, rec.calls[0].url, want)
		}
	}
}

func TestEachInstanceBindingIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{}
	instances := computev1beta1.NewInstanceClient(newClient(t, rec))
	ctx := context.Background()
	name := zone + "/instances/i-1"

	if _, err := instances.CreateInstance(ctx, &computev1beta1.CreateInstanceRequest{
		Parent: zone, Instance: &computev1beta1.Instance{InstanceType: "c1.small", ImageFamily: "imageFamilies/debian-12"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.UpdateInstance(ctx, &computev1beta1.UpdateInstanceRequest{
		Instance:   &computev1beta1.Instance{Name: name, InstanceType: "c1.large"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"instance_type", "tags"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.ListInstances(ctx, &computev1beta1.ListInstancesRequest{Parent: zone, PageSize: 5, PageToken: "next"}); err != nil {
		t.Fatal(err)
	}
	if _, err := instances.StopInstance(ctx, &computev1beta1.StopInstanceRequest{Name: name, RequestId: "given"}); err != nil {
		t.Fatal(err)
	}

	created, updated, listed, stopped := rec.calls[0], rec.calls[1], rec.calls[2], rec.calls[3]
	if q := created.url.Query(); created.method != "POST" || created.url.Path != "/v1beta1/"+zone+"/instances" ||
		!sameJSON(created.body, `{"instanceType":"c1.small","imageFamily":"imageFamilies/debian-12"}`) ||
		!uuid.MatchString(q.Get("requestId")) || len(q) != 1 {
		t.Errorf("create: %s %s %s", created.method, created.url, created.body)
	}
	if q := updated.url.Query(); updated.method != "PATCH" || updated.url.Path != "/v1beta1/"+name ||
		!sameJSON(updated.body, `{"name":"`+name+`","instanceType":"c1.large"}`) ||
		q.Get("updateMask") != "instanceType,tags" || !uuid.MatchString(q.Get("requestId")) {
		t.Errorf("update: %s %s %s", updated.method, updated.url, updated.body)
	}
	if q := listed.url.Query(); listed.method != "GET" || listed.url.Path != "/v1beta1/"+zone+"/instances" || listed.body != "" ||
		q.Get("pageSize") != "5" || q.Get("pageToken") != "next" || len(q) != 2 {
		t.Errorf("list: %s %s %s", listed.method, listed.url, listed.body)
	}
	// body: "*": the whole request is the body.
	if stopped.method != "POST" || stopped.url.Path != "/v1beta1/"+name+":stop" || stopped.url.RawQuery != "" ||
		!sameJSON(stopped.body, `{"name":"`+name+`","requestId":"given"}`) {
		t.Errorf("stop: %s %s %s", stopped.method, stopped.url, stopped.body)
	}
}

func TestCreateInstanceAnswersWithAnOperationThatWaitsForTheInstance(t *testing.T) {
	op := zone + "/operations/op-1"
	rec := &recorder{answer: func(r *http.Request) string {
		if strings.HasSuffix(r.URL.Path, ":wait") {
			return `{"name":"` + op + `","done":true,"response":{"@type":"type.googleapis.com/origincloud.compute.v1beta1.Instance","name":"` + zone + `/instances/i-1","state":"RUNNING"}}`
		}
		return `{"name":"` + op + `"}`
	}}
	instances := computev1beta1.NewInstanceClient(newClient(t, rec, core.WithRegion("eu-central1")))

	created, err := instances.CreateInstance(context.Background(), &computev1beta1.CreateInstanceRequest{Parent: zone, Instance: &computev1beta1.Instance{}})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := created.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if instance.GetName() != zone+"/instances/i-1" || instance.GetState() != computev1beta1.Instance_RUNNING {
		t.Errorf("waited for %v", instance)
	}
	if want := "https://compute.eu-central1.origincloudapis.eu/v1beta1/" + op + ":wait"; rec.calls[1].url.String() != want {
		t.Errorf("waited at %s, want %s", rec.calls[1].url, want)
	}
}

// A generated key pair's private key is in the response that created it,
// and in no other.
func TestCreateKeyPairDecodesThePrivateKey(t *testing.T) {
	rec := &recorder{answer: func(*http.Request) string {
		return `{"name":"projects/p/keyPairs/kp-1","publicKey":"ssh-ed25519 AAAA","privateKey":"-----BEGIN OPENSSH PRIVATE KEY-----\n..."}`
	}}
	keyPairs := computev1beta1.NewKeyPairClient(newClient(t, rec))

	keyPair, err := keyPairs.CreateKeyPair(context.Background(), &computev1beta1.CreateKeyPairRequest{Parent: "projects/p", KeyPair: &computev1beta1.KeyPair{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(keyPair.GetPrivateKey(), "-----BEGIN OPENSSH PRIVATE KEY-----") || keyPair.GetPublicKey() != "ssh-ed25519 AAAA" {
		t.Errorf("created %v", keyPair)
	}
	if got := rec.calls[0]; got.method != "POST" || got.url.Path != "/v1beta1/projects/p/keyPairs" || !uuid.MatchString(got.url.Query().Get("requestId")) {
		t.Errorf("create: %s %s", got.method, got.url)
	}
}
