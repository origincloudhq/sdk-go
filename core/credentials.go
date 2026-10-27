package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TokenURL is the identity provider's token endpoint, where service
// account credentials and the CLI's session are exchanged for access
// tokens.
const TokenURL = "https://auth.origincloud.eu/realms/origincloud/protocol/openid-connect/token"

const (
	envAccessToken  = "ORIGINCLOUD_ACCESS_TOKEN"
	envClientID     = "ORIGINCLOUD_CLIENT_ID"
	envClientSecret = "ORIGINCLOUD_CLIENT_SECRET"

	// The OAuth client the CLI logs in as, which its refresh token
	// belongs to.
	cliClientID = "origin-cli"

	// How long before its expiry a token is replaced, so that it doesn't
	// expire on the way.
	expiryMargin = 30 * time.Second
)

// Credentials supply the access token each call is sent with.
type Credentials interface {
	// Token returns a valid access token.
	Token(ctx context.Context) (string, error)
}

// ErrNoCredentials is returned by [DefaultCredentials] when neither the
// environment nor a CLI session holds any.
var ErrNoCredentials = errors.New("origincloud: no credentials: set ORIGINCLOUD_CLIENT_ID and ORIGINCLOUD_CLIENT_SECRET, set ORIGINCLOUD_ACCESS_TOKEN, or log in with `origin id login`")

// DefaultCredentials returns the first credentials found, in the order
// the package documentation lists.
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
	session, err := CLISession()
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoCredentials
	}
	return session, err
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
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Add(expiryMargin).Before(c.expiry) {
		return c.token, nil
	}
	token, expiry, err := renew()
	if err != nil {
		return "", err
	}
	c.token, c.expiry = token, expiry
	return token, nil
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

func requestToken(ctx context.Context, client *http.Client, endpoint string, form url.Values) (grantedToken, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if endpoint == "" {
		endpoint = TokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return grantedToken{}, fmt.Errorf("origincloud: requesting a token: %w", err)
	}
	var granted grantedToken
	if err := json.Unmarshal(body, &granted); err != nil {
		return grantedToken{}, fmt.Errorf("origincloud: requesting a token: HTTP %d, not a token response", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || granted.AccessToken == "" {
		reason := granted.ErrorDescription
		if reason == "" {
			reason = granted.Error
		}
		return grantedToken{}, fmt.Errorf("origincloud: the token request was refused (HTTP %d): %s", resp.StatusCode, reason)
	}
	return granted, nil
}
