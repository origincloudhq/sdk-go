package locationv1beta1_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.origincloud.eu/sdk/core"
	locationv1beta1 "go.origincloud.eu/sdk/location/v1beta1"
)

// recorder answers every request with body and records the URLs it was
// sent.
type recorder struct {
	body string
	urls []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL.String())
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func newClient(t *testing.T, rec *recorder, opts ...core.Option) *locationv1beta1.LocationClient {
	t.Helper()
	opts = append(opts, core.WithCredentials(core.AccessToken("t")), core.WithHTTPClient(&http.Client{Transport: rec}))
	client, err := core.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return locationv1beta1.NewLocationClient(client)
}

// The catalog is global: a client with a region still reads it from the
// global host.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	locations := newClient(t, rec, core.WithRegion("eu-central1"))
	if _, err := locations.ListZones(context.Background(), &locationv1beta1.ListZonesRequest{}); err != nil {
		t.Fatal(err)
	}
	if want := "https://location.origincloudapis.eu/v1beta1/zones"; len(rec.urls) != 1 || rec.urls[0] != want {
		t.Fatalf("called %v, want [%s]", rec.urls, want)
	}
}

func TestListZonesDecodesTheServicesEachZoneOffers(t *testing.T) {
	rec := &recorder{body: `{"zones":[{"name":"zones/eu-central1-az1","displayName":"Frankfurt 1","region":"regions/eu-central1","services":["pvs"]}]}`}
	resp, err := newClient(t, rec).ListZones(context.Background(), &locationv1beta1.ListZonesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	zones := resp.GetZones()
	if len(zones) != 1 {
		t.Fatalf("got %d zones, want 1", len(zones))
	}
	z := zones[0]
	if z.GetName() != "zones/eu-central1-az1" || z.GetRegion() != "regions/eu-central1" || len(z.GetServices()) != 1 || z.GetServices()[0] != "pvs" {
		t.Fatalf("got %v", z)
	}
}

func TestGetRegionAndGetZoneSendTheName(t *testing.T) {
	rec := &recorder{body: `{}`}
	locations := newClient(t, rec)
	ctx := context.Background()
	if _, err := locations.GetRegion(ctx, &locationv1beta1.GetRegionRequest{Name: "regions/eu-central1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := locations.GetZone(ctx, &locationv1beta1.GetZoneRequest{Name: "zones/eu-central1-az1"}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://location.origincloudapis.eu/v1beta1/regions/eu-central1",
		"https://location.origincloudapis.eu/v1beta1/zones/eu-central1-az1",
	}
	if strings.Join(rec.urls, " ") != strings.Join(want, " ") {
		t.Fatalf("called %v, want %v", rec.urls, want)
	}
}

func TestANameThatIsntAZoneIsRefusedBeforeSending(t *testing.T) {
	rec := &recorder{body: `{}`}
	locations := newClient(t, rec)
	for _, name := range []string{"", "zones/", "zones/..", "regions/eu-central1", "zones/a/b"} {
		if _, err := locations.GetZone(context.Background(), &locationv1beta1.GetZoneRequest{Name: name}); err == nil {
			t.Errorf("GetZone(%q) was sent", name)
		}
	}
	if len(rec.urls) != 0 {
		t.Fatalf("sent %v", rec.urls)
	}
}
