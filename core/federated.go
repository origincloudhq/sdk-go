package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// STSEndpoint is the security token service, where a CI job's OIDC token
// is exchanged for a workload service account's access tokens.
const STSEndpoint = "https://sts.origincloudapis.eu"

// OIDCAudience is the audience a CI job's OIDC token is asked for unless
// another is set.
const OIDCAudience = "https://sts.origincloudapis.eu"

const (
	envOIDCDisabled  = "ORIGINCLOUD_OIDC_DISABLED"
	envOIDCTokenFile = "ORIGINCLOUD_OIDC_TOKEN_FILE"
	envOIDCToken     = "ORIGINCLOUD_OIDC_TOKEN"
	envOIDCAudience  = "ORIGINCLOUD_OIDC_AUDIENCE"
	envSTSEndpoint   = "ORIGINCLOUD_STS_ENDPOINT"

	// What GitHub Actions sets in a job that may ask for an OIDC token:
	// one with `permissions: id-token: write`.
	envGitHubTokenURL     = "ACTIONS_ID_TOKEN_REQUEST_URL"
	envGitHubRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"

	// The token exchange (RFC 8693) of a JWT.
	tokenExchangeGrant = "urn:ietf:params:oauth:grant-type:token-exchange"
	jwtTokenType       = "urn:ietf:params:oauth:token-type:jwt"

	// How long before its expiry an exchanged access token is replaced.
	federatedExpiryMargin = 60 * time.Second

	// How often an exchange the security token service answers 503 is
	// sent in all, and how long to wait before sending it again the first
	// time; the wait doubles.
	stsAttempts   = 3
	stsRetryDelay = 200 * time.Millisecond
)

// FederatedCredentials exchange a CI job's OIDC token for a workload
// service account's access tokens at the security token service, as they
// expire. A federated credential on the account decides which jobs'
// tokens it trusts. Every exchange is of a fresh job token, as a job's
// tokens are short-lived, except with [FederatedFromToken], whose one token
// is exchanged again until it expires.
type FederatedCredentials struct {
	// Endpoint is the security token service; [STSEndpoint] when empty.
	Endpoint string
	// Audience is the audience the job's token is asked for;
	// [OIDCAudience] when empty. A token file, or a token handed over as
	// it is, holds a token issued for whatever audience it was asked for.
	Audience string
	// HTTPClient sends the requests for the job's token and the
	// exchanges; [http.DefaultClient] when nil.
	HTTPClient *http.Client

	subjectToken func(ctx context.Context, client *http.Client, audience string) (string, error)
	cache        tokenCache
}

// FederatedFromFile returns credentials that exchange the OIDC token held
// in the file at path, such as another CI provider's or a Kubernetes
// projected service account token. The file is read again at every
// exchange, so a rotated token is picked up.
func FederatedFromFile(path string) *FederatedCredentials {
	return &FederatedCredentials{subjectToken: func(context.Context, *http.Client, string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("origincloud: reading the OIDC token file: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", fmt.Errorf("origincloud: the OIDC token file %s is empty", path)
		}
		return token, nil
	}}
}

// FederatedFromToken returns credentials that exchange token, a CI job's
// OIDC token handed over as it is, such as one GitLab CI sets in a variable
// the job names in its id_tokens. It can't be renewed: every exchange is of
// the same token, which works until the token itself expires, and an
// expired one is an error without asking the security token service.
func FederatedFromToken(token string) *FederatedCredentials {
	return federatedFromToken(token, "")
}

// federatedFromToken is FederatedFromToken, whose errors name variable,
// the environment variable the token came from, when it is not empty.
func federatedFromToken(token, variable string) *FederatedCredentials {
	token = strings.TrimSpace(token)
	return &FederatedCredentials{subjectToken: func(context.Context, *http.Client, string) (string, error) {
		if token == "" {
			return "", errors.New("origincloud: the CI job's OIDC token is empty")
		}
		if expiry, ok := jwtExpiry(token); ok && !time.Now().Before(expiry) {
			if variable != "" {
				return "", fmt.Errorf("origincloud: the CI job's OIDC token in %s expired at %s, and a token in a variable can't be renewed: ask the CI provider for a longer-lived one, or use %s", variable, expiry.UTC().Format(time.RFC3339), envOIDCTokenFile)
			}
			return "", fmt.Errorf("origincloud: the CI job's OIDC token expired at %s, and a token handed over as it is can't be renewed: ask the CI provider for a longer-lived one, or use FederatedFromFile", expiry.UTC().Format(time.RFC3339))
		}
		return token, nil
	}}
}

// jwtExpiry is the expiry a JWT's payload claims, read without verifying
// its signature: the security token service verifies it. ok is false when
// the payload can't be read or claims no expiry.
func jwtExpiry(token string) (expiry time.Time, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp *float64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == nil {
		return time.Time{}, false
	}
	return time.Unix(int64(*claims.Exp), 0), true
}

// FederatedFromGitHubActions returns credentials that exchange the GitHub
// Actions job's OIDC token, asked for at requestURL with requestToken: the
// values of ACTIONS_ID_TOKEN_REQUEST_URL and ACTIONS_ID_TOKEN_REQUEST_TOKEN,
// which GitHub sets in a job with `permissions: id-token: write`.
func FederatedFromGitHubActions(requestURL, requestToken string) *FederatedCredentials {
	return &FederatedCredentials{subjectToken: func(ctx context.Context, client *http.Client, audience string) (string, error) {
		return gitHubActionsToken(ctx, client, requestURL, requestToken, audience)
	}}
}

// federatedFromEnvironment returns the federated credentials the
// environment names: a token file, which comes first as Kubernetes rotates
// it in place, or else a token in ORIGINCLOUD_OIDC_TOKEN, or else a GitHub
// Actions job. It returns nil when it names none, or when
// ORIGINCLOUD_OIDC_DISABLED is true.
func federatedFromEnvironment() *FederatedCredentials {
	if strings.EqualFold(os.Getenv(envOIDCDisabled), "true") {
		return nil
	}
	var c *FederatedCredentials
	requestURL, requestToken := os.Getenv(envGitHubTokenURL), os.Getenv(envGitHubRequestToken)
	switch path := os.Getenv(envOIDCTokenFile); {
	case path != "":
		c = FederatedFromFile(path)
	case os.Getenv(envOIDCToken) != "":
		c = federatedFromToken(os.Getenv(envOIDCToken), envOIDCToken)
	case requestURL != "" && requestToken != "":
		c = FederatedFromGitHubActions(requestURL, requestToken)
	default:
		return nil
	}
	c.Endpoint = os.Getenv(envSTSEndpoint)
	c.Audience = os.Getenv(envOIDCAudience)
	return c
}

// Token returns the current access token, exchanging a job token for a
// new one about a minute before it expires.
func (c *FederatedCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.getWithin(federatedExpiryMargin, func() (string, time.Time, error) {
		if c.subjectToken == nil {
			return "", time.Time{}, errors.New("origincloud: federated credentials need a job token's source: use FederatedFromFile, FederatedFromToken or FederatedFromGitHubActions")
		}
		audience := c.Audience
		if audience == "" {
			audience = OIDCAudience
		}
		subject, err := c.subjectToken(ctx, c.HTTPClient, audience)
		if err != nil {
			return "", time.Time{}, err
		}
		granted, err := c.exchange(ctx, subject)
		if err != nil {
			return "", time.Time{}, err
		}
		return granted.AccessToken, granted.expiry(), nil
	})
}

var (
	errSTSInvalidGrant = errors.New("origincloud: the security token service refused the CI job's OIDC token: check the federated credential's issuer, audience and conditions against the token's claims")
	errSTSUnavailable  = errors.New("origincloud: the security token service can't check the CI job's OIDC token now (HTTP 503): try again later")
)

// exchange exchanges the job's token for an access token. A 503 is sent
// again, up to stsAttempts in all.
func (c *FederatedCredentials) exchange(ctx context.Context, subject string) (grantedToken, error) {
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = STSEndpoint
	}
	endpoint = strings.TrimSuffix(endpoint, "/") + "/v1/token"
	form := url.Values{
		"grant_type":         {tokenExchangeGrant},
		"subject_token":      {subject},
		"subject_token_type": {jwtTokenType},
	}
	delay := stsRetryDelay
	for attempt := 1; ; attempt++ {
		status, granted, err := postToken(ctx, c.HTTPClient, endpoint, form)
		if status != http.StatusServiceUnavailable {
			if err != nil {
				return grantedToken{}, err
			}
			return granted, stsRefusal(status, granted)
		}
		if attempt == stsAttempts {
			return grantedToken{}, errSTSUnavailable
		}
		select {
		case <-ctx.Done():
			return grantedToken{}, fmt.Errorf("origincloud: exchanging the CI job's OIDC token: %w", ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// stsRefusal is why the security token service handed out no access
// token, or nil when it handed one out.
func stsRefusal(status int, granted grantedToken) error {
	switch {
	case status == http.StatusOK && granted.AccessToken != "":
		return nil
	case status == http.StatusOK:
		return errors.New("origincloud: the security token service's answer is not a token")
	case granted.Error == "invalid_grant":
		return errSTSInvalidGrant
	case granted.Error == "invalid_request":
		return fmt.Errorf("origincloud: the security token service refused the exchange as malformed: %s", granted.reason())
	default:
		return fmt.Errorf("origincloud: the security token service refused the exchange (HTTP %d): %s", status, granted.reason())
	}
}

// gitHubActionsToken asks GitHub Actions for the job's OIDC token for
// audience.
func gitHubActionsToken(ctx context.Context, client *http.Client, requestURL, requestToken, audience string) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	// The URL GitHub sets already carries a query.
	separator := "?"
	if strings.Contains(requestURL, "?") {
		separator = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL+separator+"audience="+url.QueryEscape(audience), nil)
	if err != nil {
		return "", fmt.Errorf("origincloud: asking GitHub Actions for the job's OIDC token: %w", err)
	}
	req.Header.Set("Authorization", "bearer "+requestToken)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("origincloud: asking GitHub Actions for the job's OIDC token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenBytes))
	if err != nil {
		return "", fmt.Errorf("origincloud: asking GitHub Actions for the job's OIDC token: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("origincloud: GitHub Actions handed out no OIDC token for the job (HTTP %d): check that the workflow grants `permissions: id-token: write`", resp.StatusCode)
	}
	var answer struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Value == "" {
		return "", errors.New("origincloud: GitHub Actions' answer holds no OIDC token")
	}
	return answer.Value, nil
}
