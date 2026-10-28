package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

// testClient returns a client sending every service's calls to handler.
func testClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	opts = append([]Option{WithEndpoint(server.URL), WithCredentials(AccessToken("test-token"))}, opts...)
	client, err := NewClient(context.Background(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

const unavailable = `{"error":{"code":503,"status":"UNAVAILABLE","message":"the read model is behind","details":[
	{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_UNAVAILABLE","domain":"origincloudapis.eu"},
	{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.010s"}]}}`

const badRequest = `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"invalid input on field volume.sizeGib: must be at least 1","details":[
	{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"BAD_REQUEST","domain":"origincloudapis.eu"},
	{"@type":"type.googleapis.com/google.rpc.RequestInfo","requestId":"4bf92f3577b34da6"},
	{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"volume.sizeGib","description":"must be at least 1"}]}]}}`

func TestARequestCarriesItsCredentialsAndAgent(t *testing.T) {
	var got *http.Request
	var body string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{}`)
	}, WithUserAgent("my-tool/1.2"))

	query := url.Values{"pageSize": {"10"}}
	payload, _ := structpb.NewStruct(map[string]any{"sizeGib": 5})
	err := client.Service("pvs", true).Call(context.Background(), Request{
		Method: http.MethodPost, Path: "/v1beta1/projects/p/zones/z/volumes", Query: query, Body: payload,
	}, &structpb.Struct{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Authorization") != "Bearer test-token" {
		t.Errorf("Authorization = %q", got.Header.Get("Authorization"))
	}
	if ua := got.Header.Get("User-Agent"); !strings.HasPrefix(ua, "my-tool/1.2 origincloud-sdk-go/") {
		t.Errorf("User-Agent = %q", ua)
	}
	if got.Header.Get("Content-Type") != "application/json" || strings.ReplaceAll(body, " ", "") != `{"sizeGib":5}` {
		t.Errorf("body = %q (%s)", body, got.Header.Get("Content-Type"))
	}
	if got.URL.Path != "/v1beta1/projects/p/zones/z/volumes" || got.URL.RawQuery != "pageSize=10" {
		t.Errorf("URL = %s", got.URL)
	}
}

func TestAnIdempotentCallIsRetriedWhileTheAnswerIsRetryable(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, unavailable)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})

	err := client.Service("pvs", true).Call(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &structpb.Struct{})
	if err != nil || calls.Load() != 3 {
		t.Fatalf("err = %v after %d calls, want success on the third", err, calls.Load())
	}
}

func TestACallThatIsntIdempotentIsSentOnce(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, unavailable)
	})

	err := client.Service("pvs", true).Call(context.Background(), Request{Method: http.MethodPost, Path: "/x"}, &structpb.Struct{})
	if ReasonOf(err) != "SERVICE_UNAVAILABLE" || calls.Load() != 1 {
		t.Fatalf("err = %v after %d calls, want one call", err, calls.Load())
	}
}

func TestRetriesStopAtTheMostAttempts(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, unavailable)
	}, WithMaxAttempts(2))

	err := client.Service("pvs", true).Call(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &structpb.Struct{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Retryable() || calls.Load() != 2 {
		t.Fatalf("err = %v after %d calls, want the retryable error after 2", err, calls.Load())
	}
}

func TestAnErrorThatIsntRetryableIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, badRequest)
	})

	err := client.Service("pvs", true).Call(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &structpb.Struct{})
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if calls.Load() != 1 {
		t.Errorf("%d calls, want 1", calls.Load())
	}
	want := Error{
		HTTPStatus: 400, Status: "INVALID_ARGUMENT", Reason: "BAD_REQUEST", RequestID: "4bf92f3577b34da6",
		Message: "invalid input on field volume.sizeGib: must be at least 1",
	}
	if apiErr.HTTPStatus != want.HTTPStatus || apiErr.Status != want.Status || apiErr.Reason != want.Reason ||
		apiErr.RequestID != want.RequestID || apiErr.Message != want.Message {
		t.Errorf("error = %+v, want %+v", apiErr, want)
	}
	if len(apiErr.FieldViolations) != 1 || apiErr.FieldViolations[0].Field != "volume.sizeGib" {
		t.Errorf("field violations = %+v", apiErr.FieldViolations)
	}
	if got := apiErr.Error(); got != "origincloud: BAD_REQUEST: invalid input on field volume.sizeGib: must be at least 1 (request 4bf92f3577b34da6)" {
		t.Errorf("Error() = %q", got)
	}
}

func TestAProxyErrorWithoutAStatusIsRetryable(t *testing.T) {
	var calls atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>bad gateway</html>")
			return
		}
		_, _ = io.WriteString(w, `{}`)
	})

	err := client.Service("pvs", true).Call(context.Background(), Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &structpb.Struct{})
	if err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
}

func TestTheConsistencyTokenIsKeptAndSentBack(t *testing.T) {
	var presented []string
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		presented = append(presented, r.Header.Get(ConsistencyTokenHeader))
		switch r.URL.Path {
		case "/mutate":
			w.Header().Set(ConsistencyTokenHeader, "v1.after-mutation")
			_, _ = io.WriteString(w, `{}`)
		case "/fail":
			// A failed mutation may have applied a command all the same.
			w.Header().Set(ConsistencyTokenHeader, "v1.after-failure")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, badRequest)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	})
	svc := client.Service("pvs", true)
	call := func(path string) {
		_ = svc.Call(context.Background(), Request{Method: http.MethodGet, Path: path}, &structpb.Struct{})
	}

	call("/read")
	call("/mutate")
	call("/read")
	call("/fail")
	call("/read")

	want := []string{"", "", "v1.after-mutation", "v1.after-mutation", "v1.after-failure"}
	if strings.Join(presented, ",") != strings.Join(want, ",") {
		t.Fatalf("presented %q, want %q", presented, want)
	}
	if client.ConsistencyToken() != "v1.after-failure" {
		t.Fatalf("held %q", client.ConsistencyToken())
	}
}

func TestEndpoints(t *testing.T) {
	clearCredentialEnv(t)
	for _, tc := range []struct {
		region   string
		regional bool
		want     string
	}{
		{"", true, "https://pvs.origincloudapis.eu"},
		{"eu-central1", true, "https://pvs.eu-central1.origincloudapis.eu"},
		{"eu-central1", false, "https://pvs.origincloudapis.eu"},
	} {
		client, err := NewClient(context.Background(), WithCredentials(AccessToken("t")), WithRegion(tc.region))
		if err != nil {
			t.Fatal(err)
		}
		if got := client.Service("pvs", tc.regional).base.String(); got != tc.want {
			t.Errorf("region %q, regional %v: %s, want %s", tc.region, tc.regional, got, tc.want)
		}
	}

	t.Setenv(envRegion, "eu-west1")
	client, _ := NewClient(context.Background(), WithCredentials(AccessToken("t")))
	if got := client.Service("pvs", true).base.String(); got != "https://pvs.eu-west1.origincloudapis.eu" {
		t.Errorf("ORIGINCLOUD_REGION: %s", got)
	}
	client, _ = NewClient(context.Background(), WithCredentials(AccessToken("t")), WithRegion(""))
	if got := client.Service("pvs", true).base.String(); got != "https://pvs.origincloudapis.eu" {
		t.Errorf("an empty WithRegion overrides the environment: %s", got)
	}
}

func TestPathValue(t *testing.T) {
	got, err := PathValue("name", "projects/p 1/zones/z/volumes/vol-1", "projects/*/zones/*/volumes/*")
	if err != nil || got != "projects/p%201/zones/z/volumes/vol-1" {
		t.Errorf("PathValue = %q, %v", got, err)
	}
	for _, bad := range []string{"", "projects/p/zones/z", "projects/p/zones//volumes/v", "projects/p/regions/z/volumes/v", "projects/p/zones/z/volumes/v/x"} {
		if _, err := PathValue("name", bad, "projects/*/zones/*/volumes/*"); err == nil {
			t.Errorf("PathValue(%q) succeeded", bad)
		}
	}
}

func TestQuery(t *testing.T) {
	m, _ := structpb.NewStruct(map[string]any{
		"parent":   "projects/p/zones/z",
		"pageSize": 10,
		"filter":   map[string]any{"state": "AVAILABLE", "ids": []any{"a", "b"}},
		"all":      true,
	})
	got, err := Query(m, "parent")
	if err != nil {
		t.Fatal(err)
	}
	if got.Encode() != "all=true&filter.ids=a&filter.ids=b&filter.state=AVAILABLE&pageSize=10" {
		t.Errorf("query = %s", got.Encode())
	}
}

func TestNewRequestIDIsAVersion4UUID(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for range 100 {
		id := NewRequestID()
		if !uuid.MatchString(id) || seen[id] {
			t.Fatalf("NewRequestID() = %q", id)
		}
		seen[id] = true
	}
}

func TestACancelledContextStopsRetrying(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Replace(unavailable, "0.010s", "60s", 1))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := client.Service("pvs", true).Call(ctx, Request{Method: http.MethodGet, Path: "/x", Idempotent: true}, &structpb.Struct{})
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}
