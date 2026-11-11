package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TokenURL is the identity provider's token endpoint, where service
// account credentials and the CLI's session are exchanged for access
// tokens.
const TokenURL = "https://auth.origincloud.eu/realms/origincloud/protocol/openid-connect/token"

// MetadataEndpoint is the instance's metadata service, which hands out the
// access tokens of the service account the instance runs as.
const MetadataEndpoint = "http://169.254.169.254"

const (
	envAccessToken  = "ORIGINCLOUD_ACCESS_TOKEN"
	envClientID     = "ORIGINCLOUD_CLIENT_ID"
	envClientSecret = "ORIGINCLOUD_CLIENT_SECRET"

	envMetadataDisabled = "ORIGINCLOUD_METADATA_DISABLED"
	envMetadataEndpoint = "ORIGINCLOUD_METADATA_ENDPOINT"

	// The OAuth client the CLI logs in as, which its refresh token
	// belongs to.
	cliClientID = "origin-cli"

	// How long before its expiry a token is replaced, so that it doesn't
	// expire on the way.
	expiryMargin = 30 * time.Second

	// The most of an answer to a token request that is read.
	maxTokenBytes = 1 << 20

	// How long a metadata session is asked to live: the most the
	// metadata service grants.
	metadataSessionTTL = 6 * time.Hour

	// How long before its expiry the metadata service's access token is
	// replaced; it lives five minutes.
	metadataExpiryMargin = 60 * time.Second

	// How long opening a metadata session may take, so that a program on
	// no instance learns it has no credentials quickly, and how long
	// asking for the credential may.
	metadataSessionTimeout    = 2 * time.Second
	metadataCredentialTimeout = 5 * time.Second

	// How often a request the metadata service answers 503 is sent in
	// all, and how long to wait before sending it again the first time;
	// the wait doubles.
	metadataAttempts   = 3
	metadataRetryDelay = 100 * time.Millisecond
)

// Credentials supply the access token each call is sent with.
type Credentials interface {
	// Token returns a valid access token.
	Token(ctx context.Context) (string, error)
}

// ErrNoCredentials is returned when there are none: by [DefaultCredentials]
// when neither the environment, a CI job's OIDC token nor a CLI session
// holds any and ORIGINCLOUD_METADATA_DISABLED is true, and by
// [InstanceMetadataCredentials.Token] when no Origin Cloud metadata service
// answers.
var ErrNoCredentials = errors.New("origincloud: no credentials: set ORIGINCLOUD_CLIENT_ID and ORIGINCLOUD_CLIENT_SECRET, set ORIGINCLOUD_ACCESS_TOKEN, run in a GitHub Actions job with `permissions: id-token: write`, set ORIGINCLOUD_OIDC_TOKEN_FILE or ORIGINCLOUD_OIDC_TOKEN, log in with `origin id login`, or run on an instance that runs as a service account")

// DefaultCredentials returns the first credentials found, in the order
// the package documentation lists. Neither a CI job's OIDC token nor the
// last, the instance's metadata service, is asked for here: neither makes
// a request until a token is asked for.
func DefaultCredentials(ctx context.Context) (Credentials, error) {
	if token := os.Getenv(envAccessToken); token != "" {
		return AccessToken(token), nil
	}
	id, secret := os.Getenv(envClientID), os.Getenv(envClientSecret)
	switch {
	case id != "" && secret != "":
		return ServiceAccount(id, secret), nil
	case id != "" || secret != "":
		return nil, fmt.Errorf("origincloud: %s and %s must be set together", envClientID, envClientSecret)
	}
	if federated := federatedFromEnvironment(); federated != nil {
		return federated, nil
	}
	session, err := CLISession()
	if !errors.Is(err, os.ErrNotExist) {
		return session, err
	}
	if strings.EqualFold(os.Getenv(envMetadataDisabled), "true") {
		return nil, ErrNoCredentials
	}
	metadata := InstanceMetadata()
	if endpoint := os.Getenv(envMetadataEndpoint); endpoint != "" {
		metadata.Endpoint = strings.TrimSuffix(endpoint, "/")
	}
	return metadata, nil
}

// AccessToken returns credentials that always send token. It is not
// renewed: a service account's lives five minutes.
func AccessToken(token string) Credentials {
	return staticToken(token)
}

type staticToken string

func (t staticToken) Token(context.Context) (string, error) { return string(t), nil }

// ServiceAccount returns credentials that exchange a service account's
// client ID and secret for access tokens, with the client credentials
// grant, as they expire.
func ServiceAccount(clientID, clientSecret string) Credentials {
	return &ServiceAccountCredentials{ClientID: clientID, ClientSecret: clientSecret}
}

// ServiceAccountCredentials exchange a service account's client ID and
// secret for access tokens as they expire.
type ServiceAccountCredentials struct {
	ClientID     string
	ClientSecret string
	// TokenURL is where tokens are requested; [TokenURL] when empty.
	TokenURL string
	// HTTPClient sends the token requests; [http.DefaultClient] when nil.
	HTTPClient *http.Client

	cache tokenCache
}

// Token returns the current access token, requesting a new one when it is
// about to expire.
func (c *ServiceAccountCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.get(func() (string, time.Time, error) {
		granted, err := requestToken(ctx, c.HTTPClient, c.TokenURL, url.Values{
			"grant_type":    {"client_credentials"},
			"client_id":     {c.ClientID},
			"client_secret": {c.ClientSecret},
		})
		if err != nil {
			return "", time.Time{}, err
		}
		return granted.AccessToken, granted.expiry(), nil
	})
}

// CLISession returns the session the origin CLI stored at its last login,
// renewed from its refresh token when its access token has expired. The
// renewed session stays in memory: the CLI's file is never written. The
// error wraps [os.ErrNotExist] when there is no session.
func CLISession() (*CLISessionCredentials, error) {
	path, err := cliSessionPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("origincloud: reading the CLI session: %w", err)
	}
	var stored cliSession
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("origincloud: reading the CLI session %s: %w", path, err)
	}
	c := &CLISessionCredentials{refreshToken: stored.RefreshToken, refreshExpiry: stored.RefreshExpires}
	c.cache.token, c.cache.expiry = stored.AccessToken, stored.Expires
	return c, nil
}

// CLISessionCredentials are the origin CLI's session.
type CLISessionCredentials struct {
	// TokenURL is where the session is renewed; [TokenURL] when empty.
	TokenURL string
	// HTTPClient sends the renewal; [http.DefaultClient] when nil.
	HTTPClient *http.Client

	cache         tokenCache
	refreshToken  string
	refreshExpiry time.Time
}

// Token returns the session's access token, renewing the session when the
// token is about to expire.
func (c *CLISessionCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.get(func() (string, time.Time, error) {
		if !time.Now().Before(c.refreshExpiry) {
			return "", time.Time{}, errors.New("origincloud: the CLI session has expired: log in again with `origin id login`")
		}
		granted, err := requestToken(ctx, c.HTTPClient, c.TokenURL, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {cliClientID},
			"refresh_token": {c.refreshToken},
		})
		if err != nil {
			return "", time.Time{}, fmt.Errorf("renewing the CLI session: %w", err)
		}
		if granted.RefreshToken != "" {
			c.refreshToken = granted.RefreshToken
			c.refreshExpiry = time.Now().Add(time.Duration(granted.RefreshExpiresIn) * time.Second)
		}
		return granted.AccessToken, granted.expiry(), nil
	})
}

// InstanceMetadata returns credentials that fetch the access tokens of the
// service account the instance runs as from its metadata service, as they
// expire. It makes no request until a token is asked for.
func InstanceMetadata() *InstanceMetadataCredentials {
	return &InstanceMetadataCredentials{}
}

// InstanceMetadataCredentials are the service account the instance runs as,
// whose access tokens the instance's metadata service hands out.
type InstanceMetadataCredentials struct {
	// Endpoint is the metadata service; [MetadataEndpoint] when empty.
	Endpoint string
	// HTTPClient sends the requests to the metadata service; when nil, a
	// client that never goes through a proxy, which the service refuses,
	// and gives up on a service that doesn't connect within a second.
	// Whichever client sends them, opening a session may take 2 seconds
	// and asking for the credential 5.
	HTTPClient *http.Client

	cache   tokenCache
	session tokenCache
}

// The client the metadata service is asked with by default.
var metadataHTTPClient = &http.Client{
	Transport: &http.Transport{
		// Never through a proxy, whatever HTTP_PROXY says: the service
		// refuses a request that came through one.
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: time.Second}).DialContext,
	},
}

// Token returns the instance's service account's access token, fetching a
// new one when it is about to expire. It returns [ErrNoCredentials] when no
// metadata service answers: the program runs on no instance.
func (c *InstanceMetadataCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.getWithin(metadataExpiryMargin, func() (string, time.Time, error) {
		status, body, err := c.requestCredential(ctx)
		if err == nil && status == http.StatusUnauthorized {
			// The session expired, or the host no longer knows it: open
			// another.
			c.session.drop()
			status, body, err = c.requestCredential(ctx)
		}
		if err != nil {
			return "", time.Time{}, err
		}
		switch status {
		case http.StatusOK:
		case http.StatusNotFound:
			return "", time.Time{}, errors.New("origincloud: the instance runs as no service account")
		case http.StatusForbidden:
			return "", time.Time{}, errMetadataProxy
		default:
			return "", time.Time{}, fmt.Errorf("origincloud: the metadata service answered HTTP %d", status)
		}
		var answer struct {
			AccessToken string    `json:"access_token"`
			ExpiresAt   time.Time `json:"expires_at"`
		}
		if json.Unmarshal(body, &answer) != nil || answer.AccessToken == "" || answer.ExpiresAt.IsZero() {
			return "", time.Time{}, errors.New("origincloud: the metadata service's answer is not a credential")
		}
		return answer.AccessToken, answer.ExpiresAt, nil
	})
}

var errMetadataProxy = errors.New("origincloud: the metadata service refused a request that came through a proxy")

// requestCredential asks for the service account's access token with the
// current session, opening one when there is none.
func (c *InstanceMetadataCredentials) requestCredential(ctx context.Context) (int, []byte, error) {
	session, err := c.session.get(func() (string, time.Time, error) {
		return c.openSession(ctx)
	})
	if err != nil {
		return 0, nil, err
	}
	return c.ask(ctx, metadataCredentialTimeout, http.MethodGet, "/latest/meta-data/service-account/token", "X-Origincloud-Metadata-Token", session)
}

// openSession opens a metadata session. Only Origin Cloud's metadata
// service answers its request with a session: another cloud's, at the same
// address, refuses a request it doesn't know, and an address that is no
// instance's is refused too, so a refusal is [ErrNoCredentials] unless it
// is one only Origin Cloud's service gives.
func (c *InstanceMetadataCredentials) openSession(ctx context.Context) (string, time.Time, error) {
	expiry := time.Now().Add(metadataSessionTTL)
	ttl := strconv.Itoa(int(metadataSessionTTL / time.Second))
	status, body, err := c.ask(ctx, metadataSessionTimeout, http.MethodPut, "/latest/api/token", "X-Origincloud-Metadata-Token-TTL-Seconds", ttl)
	if err != nil {
		return "", time.Time{}, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusForbidden:
		return "", time.Time{}, errMetadataProxy
	case http.StatusServiceUnavailable:
		return "", time.Time{}, fmt.Errorf("origincloud: the metadata service refused a session (HTTP %d)", status)
	default:
		return "", time.Time{}, ErrNoCredentials
	}
	session := strings.TrimSpace(string(body))
	if session == "" {
		return "", time.Time{}, errors.New("origincloud: the metadata service's answer is not a credential")
	}
	return session, expiry, nil
}

// ask sends the metadata service a request with one header, giving it
// timeout, and returns the answer's status and body. A 503 is sent again,
// up to metadataAttempts in all. A service that doesn't answer is
// [ErrNoCredentials].
func (c *InstanceMetadataCredentials) ask(ctx context.Context, timeout time.Duration, method, path, header, value string) (int, []byte, error) {
	delay := metadataRetryDelay
	for attempt := 1; ; attempt++ {
		status, body, err := c.askOnce(ctx, timeout, method, path, header, value)
		if err != nil || status != http.StatusServiceUnavailable || attempt == metadataAttempts {
			return status, body, err
		}
		select {
		case <-ctx.Done():
			return 0, nil, fmt.Errorf("origincloud: asking the metadata service: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func (c *InstanceMetadataCredentials) askOnce(ctx context.Context, timeout time.Duration, method, path, header, value string) (int, []byte, error) {
	client := c.HTTPClient
	if client == nil {
		client = metadataHTTPClient
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = MetadataEndpoint
	}
	limited, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(limited, method, strings.TrimSuffix(endpoint, "/")+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("origincloud: asking the metadata service: %w", err)
	}
	req.Header.Set(header, value)
	resp, err := client.Do(req)
	if err != nil {
		// The caller's deadline or cancellation is its own, not proof
		// that there is no metadata service.
		if ctx.Err() != nil {
			return 0, nil, fmt.Errorf("origincloud: asking the metadata service: %w", err)
		}
		return 0, nil, ErrNoCredentials
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes))
	if err != nil {
		return 0, nil, fmt.Errorf("origincloud: asking the metadata service: %w", err)
	}
	return resp.StatusCode, body, nil
}

// The file the CLI stores its session in.
type cliSession struct {
	AccessToken    string    `json:"access_token"`
	Expires        time.Time `json:"expires"`
	RefreshToken   string    `json:"refresh_token"`
	RefreshExpires time.Time `json:"refresh_expires"`
}

// Where the CLI stores its session: under XDG_DATA_HOME, or
// ~/.local/share, on every platform.
func cliSessionPath() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("origincloud: finding the CLI session: %w", err)
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "origincloud", "session.json"), nil
}

// One access token, replaced once it is about to expire.
type tokenCache struct {
	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (c *tokenCache) get(renew func() (string, time.Time, error)) (string, error) {
	return c.getWithin(expiryMargin, renew)
}

// getWithin is get, replacing the token margin before its expiry.
func (c *tokenCache) getWithin(margin time.Duration, renew func() (string, time.Time, error)) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(margin).Before(c.expiry) {
		return c.token, nil
	}
	token, expiry, err := renew()
	if err != nil {
		return "", err
	}
	c.token, c.expiry = token, expiry
	return token, nil
}

// drop forgets the token, so that the next get renews it.
func (c *tokenCache) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = ""
}

// A token endpoint's answer.
type grantedToken struct {
	AccessToken      string `json:"access_token"`
	ExpiresIn        int64  `json:"expires_in"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresIn int64  `json:"refresh_expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (g grantedToken) expiry() time.Time {
	return time.Now().Add(time.Duration(g.ExpiresIn) * time.Second)
}

// reason is why the token endpoint refused: its description, or its code.
func (g grantedToken) reason() string {
	if g.ErrorDescription != "" {
		return g.ErrorDescription
	}
	return g.Error
}

func requestToken(ctx context.Context, client *http.Client, endpoint string, form url.Values) (grantedToken, error) {
	if endpoint == "" {
		endpoint = TokenURL
	}
	status, granted, err := postToken(ctx, client, endpoint, form)
	if err != nil {
		return grantedToken{}, err
	}
	if status != http.StatusOK || granted.AccessToken == "" {
		return grantedToken{}, fmt.Errorf("origincloud: the token request was refused (HTTP %d): %s", status, granted.reason())
	}
	return granted, nil
}

// postToken sends a token request and returns the answer's status and the
// answer read as a token endpoint's. An answer that is not one is an error
// with its status.
func postToken(ctx context.Context, client *http.Client, endpoint string, form url.Values) (int, grantedToken, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes))
	if err != nil {
		return 0, grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	var granted grantedToken
	if err := json.Unmarshal(body, &granted); err != nil {
		return resp.StatusCode, grantedToken{}, fmt.Errorf("origincloud: requesting a token: HTTP %d, not a token response", resp.StatusCode)
	}
	return resp.StatusCode, granted, nil
}
