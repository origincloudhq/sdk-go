package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ConsistencyTokenHeader carries the consistency token, in responses and
// in requests.
const ConsistencyTokenHeader = "Origin-Consistency-Token"

const (
	// The most of a response body that is read.
	maxResponseBytes = 32 << 20

	firstRetryDelay = 200 * time.Millisecond
	maxRetryDelay   = 10 * time.Second
)

var (
	marshalOptions = protojson.MarshalOptions{}
	// A newer API may answer with fields this SDK doesn't know yet.
	unmarshalOptions = protojson.UnmarshalOptions{DiscardUnknown: true}
)

// A Service is the endpoint of one service, which its generated client
// sends calls to.
type Service struct {
	client *Client
	base   *url.URL
	// The service's shortcode, such as "pvs": its host's first label, and
	// the service a full resource name in a path field must name.
	shortcode string
}

// Client returns the client the service's calls go through.
func (s *Service) Client() *Client { return s.client }

// A Request is one call of a service's method, as its HTTP binding sends
// it. Generated clients build it.
type Request struct {
	Method string
	// Path is the escaped path, from the root.
	Path  string
	Query url.Values
	// Body is sent as JSON when not nil.
	Body proto.Message
	// Idempotent says that sending the call twice has the effect of
	// sending it once: a read, or a mutation with a request id. Only an
	// idempotent call is retried.
	Idempotent bool
	// Unauthenticated sends the call without the client's credentials, to
	// a method that authenticates no caller, such as the security token
	// service's: it then works with none.
	Unauthenticated bool
}

// Call sends r, retrying it while that is safe and the answer allows it,
// and reads the answer into out.
func (s *Service) Call(ctx context.Context, r Request, out proto.Message) error {
	got, err := s.call(ctx, r, "application/json")
	if err != nil {
		return err
	}
	if err := unmarshalOptions.Unmarshal(got.body, out); err != nil {
		return fmt.Errorf("origincloud: reading the response to %s %s: %w", r.Method, r.Path, err)
	}
	return nil
}

// CallHTTPBody sends r like Call, to a method that answers with a file
// rather than JSON, a google.api.HttpBody: it returns the file as it is,
// its bytes in Data and their media type, such as "application/pdf", in
// ContentType. A refused call is an [*Error] all the same.
func (s *Service) CallHTTPBody(ctx context.Context, r Request) (*httpbody.HttpBody, error) {
	got, err := s.call(ctx, r, "*/*")
	if err != nil {
		return nil, err
	}
	return &httpbody.HttpBody{ContentType: got.contentType, Data: got.body}, nil
}

// An answer: the body of a 200, and its media type.
type answer struct {
	body        []byte
	contentType string
}

// call sends r, asking for the media types accept names, retrying it while
// that is safe and the answer allows it.
func (s *Service) call(ctx context.Context, r Request, accept string) (answer, error) {
	var body []byte
	if r.Body != nil {
		var err error
		if body, err = marshalOptions.Marshal(r.Body); err != nil {
			return answer{}, fmt.Errorf("origincloud: encoding the request: %w", err)
		}
	}
	for attempt := 1; ; attempt++ {
		got, err := s.send(ctx, r, body, accept)
		if err == nil {
			return got, nil
		}
		if !r.Idempotent || attempt >= s.client.maxAttempts {
			return answer{}, err
		}
		delay, retry := retryDelay(err, attempt)
		if !retry {
			return answer{}, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return answer{}, err
		case <-timer.C:
		}
	}
}

// send makes one attempt, and answers with the body of a 200 and its
// media type.
func (s *Service) send(ctx context.Context, r Request, body []byte, accept string) (answer, error) {
	target := *s.base
	target.RawPath = s.base.EscapedPath() + r.Path
	path, err := url.PathUnescape(target.RawPath)
	if err != nil {
		return answer{}, fmt.Errorf("origincloud: %w", err)
	}
	target.Path = path
	target.RawQuery = r.Query.Encode()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), reader)
	if err != nil {
		return answer{}, fmt.Errorf("origincloud: %w", err)
	}
	if !r.Unauthenticated {
		token, err := s.client.credentials.Token(ctx)
		if err != nil {
			return answer{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", s.client.userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if held := s.client.ConsistencyToken(); held != "" {
		req.Header.Set(ConsistencyTokenHeader, held)
	}
	resp, err := s.client.httpClient.Do(req)
	if err != nil {
		return answer{}, &transportError{err: err}
	}
	defer resp.Body.Close()
	// A failed mutation may still have applied a command: its token comes
	// back either way.
	if answered := resp.Header.Get(ConsistencyTokenHeader); answered != "" {
		s.client.SetConsistencyToken(answered)
	}
	// One byte more than the most, to tell a body cut off at the limit
	// from one that ends there.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return answer{}, &transportError{err: err}
	}
	if resp.StatusCode != http.StatusOK {
		return answer{}, errorFromResponse(resp.StatusCode, data)
	}
	if len(data) > maxResponseBytes {
		return answer{}, fmt.Errorf("origincloud: the response to %s %s is larger than %d MiB", r.Method, r.Path, maxResponseBytes>>20)
	}
	return answer{body: data, contentType: resp.Header.Get("Content-Type")}, nil
}

// A call that got no answer: the connection failed or broke off.
type transportError struct{ err error }

func (e *transportError) Error() string { return "origincloud: " + e.err.Error() }
func (e *transportError) Unwrap() error { return e.err }

// retryDelay says whether err may go away if the call is sent again, and
// how long to wait before attempt+1: what the answer asks for, or a
// jittered delay that doubles with each attempt.
func retryDelay(err error, attempt int) (time.Duration, bool) {
	var asked time.Duration
	switch e := err.(type) {
	case *transportError:
	case *Error:
		if !e.retryable {
			return 0, false
		}
		asked = e.RetryDelay
	default:
		return 0, false
	}
	backoff := firstRetryDelay << (attempt - 1)
	if backoff > maxRetryDelay || backoff <= 0 {
		backoff = maxRetryDelay
	}
	backoff = backoff/2 + mrand.N(backoff/2+1)
	return max(asked, backoff), true
}

// Query returns the fields of m as query parameters, by their JSON paths,
// leaving out the fields named by skip and fields at their default value.
// A generated client sends the fields its method's path and body don't
// carry this way.
func Query(m proto.Message, skip ...string) (url.Values, error) {
	encoded, err := marshalOptions.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("origincloud: encoding the request: %w", err)
	}
	return queryFromJSON(encoded, skip)
}

// RelativeName returns the relative name value, the path field field of a
// call to the service, stands for: value itself, or the relative name of a
// full resource name it is. Generated clients call it, and send the
// relative name in the path and in the body alike.
//
// services are the short codes of the services whose full names the field
// takes: the service of the type the field refers to, such as
// "resourcemanager" for a billing account's parent (organizations/*),
// [AnyService] for a field referring to any type, or none for the
// service's own. A full name of one of them, such as
// "//resourcemanager.origincloudapis.eu/organizations/o", stands for its
// relative name. A full name of another service, one with a regional or
// zonal host or an API version, a URN, a URL or a malformed full name is
// refused with a [*FullNameError] naming the hosts the field takes.
func (s *Service) RelativeName(field, value string, services ...string) (string, error) {
	return pathName(s.shortcode, services, field, value)
}

// PathValue returns value, the path field field of a call to the service,
// as the path segments of the request: its [Service.RelativeName], checked
// against pattern and escaped as the function [PathValue] does.
func (s *Service) PathValue(field, value, pattern string, services ...string) (string, error) {
	relative, err := s.RelativeName(field, value, services...)
	if err != nil {
		return "", err
	}
	return PathValue(field, relative, pattern)
}

// PathValue returns value, a relative resource name, as the path segments
// of a request, checking it has the form pattern, such as
// "projects/*/zones/*/volumes/*", gives it. field names it in the error.
// [Service.PathValue] also takes a full resource name of its service.
func PathValue(field, value, pattern string) (string, error) {
	got := strings.Split(value, "/")
	want := strings.Split(pattern, "/")
	valid := len(got) == len(want)
	for i := 0; valid && i < len(want); i++ {
		switch want[i] {
		case "*":
			// A dot segment would move the request to another path, which
			// the server would act on instead.
			valid = got[i] != "" && got[i] != "." && got[i] != ".."
		default:
			valid = got[i] == want[i]
		}
	}
	if !valid {
		return "", fmt.Errorf("origincloud: %s must have the form %s, got %q", field, pattern, value)
	}
	for i, segment := range got {
		got[i] = url.PathEscape(segment)
	}
	return strings.Join(got, "/"), nil
}

// NewRequestID returns a new random request id, a UUID, for a mutation
// whose caller left its request id empty.
func NewRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
