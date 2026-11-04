package billingv1beta1_test

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
	"time"

	billingv1beta1 "go.origincloud.eu/sdk/billing/v1beta1"
	"go.origincloud.eu/sdk/core"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/timestamppb"
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

// Billing is global: a client with a region still calls the global host,
// the price list included.
func TestARegionalClientCallsTheGlobalHost(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec, core.WithRegion("eu-central1"))
	ctx := context.Background()
	if _, err := billingv1beta1.NewBillingAccountClient(client).GetProjectBillingInfo(ctx, &billingv1beta1.GetProjectBillingInfoRequest{Name: "projects/p/billingInfo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := billingv1beta1.NewPriceListClient(client).GetPriceList(ctx, &billingv1beta1.GetPriceListRequest{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://billing.origincloudapis.eu/v1beta1/projects/p/billingInfo",
		"https://billing.origincloudapis.eu/v1/pricing",
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

// The price list answers with its fields' proto names, not their JSON
// names, and leaves out a price's end while it has none.
func TestThePriceListDecodesAsTheServiceAnswersIt(t *testing.T) {
	rec := &recorder{body: `{"currency":"EUR","amounts_exclude_vat":true,"last_changed":"2026-09-22T19:42:11.021738Z",` +
		`"families":[{"family":"compute_instance","prices":[{"product_key":"compute.gp1.core.2",` +
		`"dimensions":{"instance_type":"gp1.core.2"},"unit_type":"seconds","unit_count":3600,"amount":"0.0551",` +
		`"valid_from":"2026-08-11T08:37:24.666437Z"}]}]}`}
	list, err := billingv1beta1.NewPriceListClient(newClient(t, rec)).GetPriceList(context.Background(), &billingv1beta1.GetPriceListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if list.GetCurrency() != "EUR" || !list.GetAmountsExcludeVat() || list.GetLastChanged().AsTime().Year() != 2026 || len(list.GetFamilies()) != 1 {
		t.Fatalf("got %v", list)
	}
	price := list.GetFamilies()[0].GetPrices()[0]
	if price.GetProductKey() != "compute.gp1.core.2" || price.GetDimensions()["instance_type"] != "gp1.core.2" ||
		price.GetUnitType() != "seconds" || price.GetUnitCount() != 3600 || price.GetAmount() != "0.0551" ||
		price.GetValidFrom() == nil || price.GetValidUntil() != nil {
		t.Errorf("got %v", price)
	}
	if got := rec.calls[0]; got.method != "GET" || got.body != "" || len(got.url.Query()) != 0 {
		t.Errorf("get: %s %s %s", got.method, got.url, got.body)
	}
}

func TestCostAndSpendLimitCallsAreSentAsTheirHTTPRulesSay(t *testing.T) {
	rec := &recorder{body: `{}`}
	client := newClient(t, rec)
	ctx := context.Background()
	start := timestamppb.New(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	end := timestamppb.New(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	limit := "250.00"

	if _, err := billingv1beta1.NewCostClient(client).QueryProjectCost(ctx, &billingv1beta1.QueryProjectCostRequest{
		Project: "projects/p", StartTime: start, EndTime: end, Granularity: billingv1beta1.Granularity_DAILY,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := billingv1beta1.NewSpendLimitClient(client).UpdateSpendLimit(ctx, &billingv1beta1.UpdateSpendLimitRequest{
		SpendLimit: &billingv1beta1.SpendLimit{Name: "organizations/o/billingAccounts/b/spendLimit", Limit: &limit, AlertThresholdsPercent: []int32{50, 90}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"limit", "alert_thresholds_percent"}},
	}); err != nil {
		t.Fatal(err)
	}

	cost, updated := rec.calls[0], rec.calls[1]
	if q := cost.url.Query(); cost.method != "GET" || cost.url.Path != "/v1beta1/projects/p:queryCost" || cost.body != "" ||
		q.Get("startTime") != "2026-11-01T00:00:00Z" || q.Get("endTime") != "2026-11-04T00:00:00Z" || q.Get("granularity") != "DAILY" || len(q) != 3 {
		t.Errorf("cost: %s %s %s", cost.method, cost.url, cost.body)
	}
	if q := updated.url.Query(); updated.method != "PATCH" || updated.url.Path != "/v1beta1/organizations/o/billingAccounts/b/spendLimit" ||
		!sameJSON(updated.body, `{"name":"organizations/o/billingAccounts/b/spendLimit","limit":"250.00","alertThresholdsPercent":[50,90]}`) ||
		q.Get("updateMask") != "limit,alertThresholdsPercent" || len(q) != 1 {
		t.Errorf("update: %s %s %s", updated.method, updated.url, updated.body)
	}
}

// A create without a request id gets one, so a retry after a lost answer
// isn't a second account.
func TestCreateBillingAccountIsSentWithARequestID(t *testing.T) {
	rec := &recorder{body: `{}`}
	if _, err := billingv1beta1.NewBillingAccountClient(newClient(t, rec)).CreateBillingAccount(context.Background(), &billingv1beta1.CreateBillingAccountRequest{
		Parent: "organizations/o", BillingAccount: &billingv1beta1.BillingAccount{DisplayName: "Research"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := rec.calls[0]; got.method != "POST" || got.url.Path != "/v1beta1/organizations/o/billingAccounts" ||
		!sameJSON(got.body, `{"displayName":"Research"}`) || !uuid.MatchString(got.url.Query().Get("requestId")) || len(got.url.Query()) != 1 {
		t.Errorf("create: %s %s %s", got.method, got.url, got.body)
	}
}
