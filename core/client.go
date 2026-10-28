package core

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// Version is this SDK's version, sent in the User-Agent header.
const Version = "0.1.0"

const (
	envRegion = "ORIGINCLOUD_REGION"

	defaultMaxAttempts = 5
)

// A Client holds what the clients of every service share: credentials,
// endpoints, an HTTP client and the consistency token. It is safe for
// concurrent use.
type Client struct {
	httpClient  *http.Client
	credentials Credentials
	region      string
	endpoint    *url.URL
	userAgent   string
	maxAttempts int

	mu    sync.Mutex
	token string
}

// An Option configures a [Client].
type Option func(*settings)

type settings struct {
	credentials Credentials
	region      *string
	endpoint    string
	httpClient  *http.Client
	userAgent   string
	maxAttempts int
}

// WithCredentials sets the credentials, instead of taking them from the
// environment or the CLI's session.
func WithCredentials(credentials Credentials) Option {
	return func(s *settings) { s.credentials = credentials }
}

// WithRegion sends calls to a region's front door,
// {service}.{region}.origincloudapis.eu, for the services that have one.
// It overrides ORIGINCLOUD_REGION; an empty region uses the regionless
// endpoint.
func WithRegion(region string) Option {
	return func(s *settings) { s.region = &region }
}

// WithEndpoint sends the calls of every service to one base URL, such as
// a test server's, instead of the service's own host.
func WithEndpoint(endpoint string) Option {
	return func(s *settings) { s.endpoint = endpoint }
}

// WithHTTPClient sets the HTTP client calls are sent with; by default it
// is [http.DefaultClient]. Its timeout, if any, bounds each attempt; the
// context bounds a call.
func WithHTTPClient(client *http.Client) Option {
	return func(s *settings) { s.httpClient = client }
}

// WithUserAgent adds a product, such as "my-tool/1.2", to the User-Agent
// header.
func WithUserAgent(product string) Option {
	return func(s *settings) { s.userAgent = product }
}

// WithMaxAttempts sets how often a retryable call is sent at most, the
// first attempt included; 1 turns retries off. The default is 5.
func WithMaxAttempts(attempts int) Option {
	return func(s *settings) { s.maxAttempts = attempts }
}

// NewClient returns a client configured by opts and the environment. It
// fails when no credentials are found or an option is invalid.
func NewClient(ctx context.Context, opts ...Option) (*Client, error) {
	var s settings
	for _, opt := range opts {
		opt(&s)
	}
	c := &Client{
		httpClient:  s.httpClient,
		credentials: s.credentials,
		maxAttempts: s.maxAttempts,
		userAgent:   "origincloud-sdk-go/" + Version,
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	if c.maxAttempts == 0 {
		c.maxAttempts = defaultMaxAttempts
	}
	if c.maxAttempts < 1 {
		return nil, fmt.Errorf("origincloud: the most attempts must be at least 1, got %d", c.maxAttempts)
	}
	if s.userAgent != "" {
		c.userAgent = s.userAgent + " " + c.userAgent
	}
	if s.region != nil {
		c.region = *s.region
	} else {
		c.region = os.Getenv(envRegion)
	}
	if s.endpoint != "" {
		endpoint, err := url.Parse(strings.TrimSuffix(s.endpoint, "/"))
		if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
			return nil, fmt.Errorf("origincloud: the endpoint must be an absolute URL, got %q", s.endpoint)
		}
		c.endpoint = endpoint
	}
	if c.credentials == nil {
		credentials, err := DefaultCredentials(ctx)
		if err != nil {
			return nil, err
		}
		c.credentials = credentials
	}
	return c, nil
}

// Service returns the endpoint of the service whose host starts with
// shortcode, such as "pvs". A regional service answers at its region's
// front door when the client has a region; a global one always at its
// regionless host. Service is called by the generated clients.
func (c *Client) Service(shortcode string, regional bool) *Service {
	base := c.endpoint
	if base == nil {
		host := shortcode + ".origincloudapis.eu"
		if regional && c.region != "" {
			host = shortcode + "." + c.region + ".origincloudapis.eu"
		}
		base = &url.URL{Scheme: "https", Host: host}
	}
	return &Service{client: c, base: base}
}

// ConsistencyToken returns the consistency token the client holds: the
// one the last mutation or finished operation answered with, or empty
// before any. Hand it to [Client.SetConsistencyToken] of a client in
// another process for its reads to show this client's writes.
func (c *Client) ConsistencyToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// SetConsistencyToken replaces the consistency token the client sends with
// its calls. The API merges a mutation's position into the token the
// mutation presented, so a client keeps every write it made since.
func (c *Client) SetConsistencyToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
}
