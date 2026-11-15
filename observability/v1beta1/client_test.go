package observabilityv1beta1_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.origincloud.eu/sdk/core"
	observabilityv1beta1 "go.origincloud.eu/sdk/observability/v1beta1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// recorder answers every request with body and records the requests it
// was sent.
type recorder struct {
	body string
	urls []*url.URL
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.urls = append(r.urls, req.URL)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(r.body)),
		Request:    req,
	}, nil
}

func newClient(t *testing.T, rec *recorder, opts ...core.Option) *observabilityv1beta1.MetricClient {
	t.Helper()
	opts = append(opts, core.WithCredentials(core.AccessToken("t")), core.WithHTTPClient(&http.Client{Transport: rec}))
	client, err := core.NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return observabilityv1beta1.NewMetricClient(client)
}

const instance = "projects/p/zones/eu-central1-az1/instances/i-1"

func query() *observabilityv1beta1.QueryTimeSeriesRequest {
	return &observabilityv1beta1.QueryTimeSeriesRequest{
		Parent:    "projects/p",
		Resource:  instance,
		Metrics:   []string{"oc_compute_instance_cpu_seconds_total", "oc_compute_instance_network_receive_bytes_total"},
		StartTime: timestamppb.New(time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)),
		EndTime:   timestamppb.New(time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)),
		Period:    durationpb.New(5 * time.Minute),
		Statistic: observabilityv1beta1.QueryTimeSeriesRequest_RATE,
	}
}

// Observability is regional: a client with a region calls its region's
// host, one without the global host.
func TestAQueryGoesToTheRegionsHost(t *testing.T) {
	for region, want := range map[string]string{
		"eu-central1": "observability.eu-central1.origincloudapis.eu",
		"":            "observability.origincloudapis.eu",
	} {
		rec := &recorder{body: `{}`}
		if _, err := newClient(t, rec, core.WithRegion(region)).QueryTimeSeries(context.Background(), query()); err != nil {
			t.Fatal(err)
		}
		if got := rec.urls[0].Host; got != want {
			t.Errorf("region %q: called %s, want %s", region, got, want)
		}
	}
}

// Every field is a query parameter: each metric one, the times as RFC 3339
// and the period as a JSON duration.
func TestAQueryIsSentAsItsHTTPRuleSays(t *testing.T) {
	rec := &recorder{body: `{}`}
	if _, err := newClient(t, rec, core.WithRegion("")).QueryTimeSeries(context.Background(), query()); err != nil {
		t.Fatal(err)
	}
	got := rec.urls[0]
	want := url.Values{
		"resource":  {instance},
		"metrics":   {"oc_compute_instance_cpu_seconds_total", "oc_compute_instance_network_receive_bytes_total"},
		"startTime": {"2026-11-04T09:00:00Z"},
		"endTime":   {"2026-11-04T10:00:00Z"},
		"period":    {"300s"},
		"statistic": {"RATE"},
	}
	if got.Path != "/v1beta1/projects/p/timeSeries:query" || !reflect.DeepEqual(got.Query(), want) {
		t.Errorf("called %s, want the query %v", got, want)
	}
}

func TestTheTimeSeriesDecodeTheirPoints(t *testing.T) {
	rec := &recorder{body: `{"timeSeries":[{"metric":"oc_compute_instance_cpu_seconds_total","resource":"` + instance + `",` +
		`"unit":"s/s","points":[{"endTime":"2026-11-04T09:05:00Z","value":0.25},{"endTime":"2026-11-04T09:10:00Z","value":0.5}]}]}`}
	got, err := newClient(t, rec).QueryTimeSeries(context.Background(), query())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.GetTimeSeries()) != 1 {
		t.Fatalf("got %v", got)
	}
	series := got.GetTimeSeries()[0]
	points := series.GetPoints()
	if series.GetMetric() != "oc_compute_instance_cpu_seconds_total" || series.GetResource() != instance || series.GetUnit() != "s/s" ||
		len(points) != 2 || points[1].GetValue() != 0.5 || !points[1].GetEndTime().AsTime().Equal(time.Date(2026, 11, 4, 9, 10, 0, 0, time.UTC)) {
		t.Errorf("got %v", series)
	}
}

// A query's parent is a project, resourcemanager's: its full name is sent
// as its relative name, the same request as projects/p makes. A full name
// on any other host is refused before sending, naming the host the field
// takes: the client's own service's is no project's.
func TestAParentTakesTheFullNameOfTheProjectItRefersTo(t *testing.T) {
	rec := &recorder{body: `{}`}
	metrics := newClient(t, rec)
	ctx := context.Background()

	for _, parent := range []string{"projects/p", "//resourcemanager.origincloudapis.eu/projects/p"} {
		req := query()
		req.Parent = parent
		if _, err := metrics.QueryTimeSeries(ctx, req); err != nil {
			t.Fatalf("%s: %v", parent, err)
		}
		if req.GetParent() != parent {
			t.Errorf("the caller's request was changed: %q", req.GetParent())
		}
	}
	if len(rec.urls) != 2 || rec.urls[0].String() != rec.urls[1].String() || rec.urls[1].EscapedPath() != "/v1beta1/projects/p/timeSeries:query" {
		t.Fatalf("sent %v", rec.urls)
	}

	for _, parent := range []string{
		"//pvs.origincloudapis.eu/projects/p",
		"//observability.origincloudapis.eu/projects/p",
		"//resourcemanager.eu-central1.origincloudapis.eu/projects/p",
	} {
		req := query()
		req.Parent = parent
		_, err := metrics.QueryTimeSeries(ctx, req)
		if err == nil || !strings.Contains(err.Error(), "parent must be a name on resourcemanager.origincloudapis.eu") {
			t.Errorf("%s: %v", parent, err)
		}
	}
	req := query()
	req.Parent = "//pvs.origincloudapis.eu/projects/p"
	if _, err := metrics.QueryTimeSeries(ctx, req); !errors.Is(err, core.ErrOtherService) {
		t.Errorf("pvs: %v", err)
	}
	if len(rec.urls) != 2 {
		t.Fatalf("sent %d calls", len(rec.urls))
	}
}
