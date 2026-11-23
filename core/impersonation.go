package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const (
	envImpersonateServiceAccount = "ORIGINCLOUD_IMPERSONATE_SERVICE_ACCOUNT"

	// The token type of an access token (RFC 8693, 3).
	accessTokenType = "urn:ietf:params:oauth:token-type:access_token"
)

// ImpersonatedCredentials act as a service account: they exchange the
// access token their source gives for the account's at the security token
// service, and exchange again before that token expires. The security token
// service hands the account's token out only to a person who holds
// iam.service_account.get_access_token on it (the role
// roles/iam.service-account-token-creator); the token names the account as
// its subject and the person as the one acting, and so does the audit log.
// A refused exchange is an error naming the account: the source's own token
// is never sent in its place.
type ImpersonatedCredentials struct {
	// Endpoint is the security token service; [STSEndpoint] when empty. It
	// must be https, or http to the loopback interface: the source's token
	// is sent to it.
	Endpoint string
	// HTTPClient sends the exchanges; [http.DefaultClient] when nil.
	HTTPClient *http.Client

	source         Credentials
	serviceAccount string
	cache          tokenCache
}

// Impersonate returns credentials that act as serviceAccount, named by its
// full name, //iam.origincloudapis.eu/organizations/{organization}/serviceAccounts/{service_account},
// with the access tokens source gives: a person's, such as the origin CLI's
// session. It makes no request until a token is asked for.
func Impersonate(source Credentials, serviceAccount string) (*ImpersonatedCredentials, error) {
	if source == nil {
		return nil, errors.New("origincloud: impersonating a service account needs credentials to impersonate it with")
	}
	name, err := ParseFullName(serviceAccount)
	if err != nil {
		return nil, fmt.Errorf("origincloud: the service account to impersonate: %w", err)
	}
	if principal, ok := name.Principal(); !ok || principal.Kind != PrincipalServiceAccount {
		return nil, fmt.Errorf("origincloud: the service account to impersonate must be named //iam.origincloudapis.eu/organizations/{organization}/serviceAccounts/{service_account}, got %q", serviceAccount)
	}
	return &ImpersonatedCredentials{source: source, serviceAccount: name.String()}, nil
}

// ServiceAccount returns the full name of the service account the
// credentials act as.
func (c *ImpersonatedCredentials) ServiceAccount() string { return c.serviceAccount }

// Token returns the service account's current access token, exchanging the
// source's token for a new one before it expires: a minute before, or
// halfway through a shorter lifetime, as the token lives no longer than the
// source's it was exchanged for.
func (c *ImpersonatedCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.getWithin(0, func() (string, time.Time, error) {
		if c.source == nil {
			return "", time.Time{}, errors.New("origincloud: impersonated credentials need a service account and a source: use Impersonate")
		}
		endpoint := c.Endpoint
		if endpoint == "" {
			endpoint = STSEndpoint
		}
		if parsed, err := url.Parse(endpoint); err != nil || !secureEndpoint(parsed) {
			return "", time.Time{}, fmt.Errorf("origincloud: the security token service %q to impersonate %s at is not an https URL", endpoint, c.serviceAccount)
		}
		subject, err := c.source.Token(ctx)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("origincloud: getting the token to impersonate %s with: %w", c.serviceAccount, err)
		}
		issued := time.Now()
		status, granted, err := exchangeAtSTS(ctx, c.HTTPClient, endpoint, url.Values{
			"grant_type":         {tokenExchangeGrant},
			"subject_token":      {subject},
			"subject_token_type": {accessTokenType},
			"service_account":    {c.serviceAccount},
		})
		if err != nil {
			return "", time.Time{}, err
		}
		if err := impersonationRefusal(status, granted, c.serviceAccount); err != nil {
			return "", time.Time{}, err
		}
		return granted.AccessToken, renewalTime(issued, time.Duration(granted.ExpiresIn)*time.Second), nil
	})
}

// renewalTime is when a token issued at issued for lifetime is replaced:
// federatedExpiryMargin before it expires, or halfway through a lifetime
// shorter than twice that.
func renewalTime(issued time.Time, lifetime time.Duration) time.Time {
	return issued.Add(lifetime - min(federatedExpiryMargin, lifetime/2))
}

// impersonationRefusal is why the security token service handed out no
// access token of serviceAccount, or nil when it handed one out.
func impersonationRefusal(status int, granted grantedToken, serviceAccount string) error {
	switch {
	case status == http.StatusOK && granted.AccessToken != "":
		return nil
	case status == http.StatusOK:
		return errors.New("origincloud: the security token service's answer is not a token")
	case status == http.StatusServiceUnavailable:
		return fmt.Errorf("origincloud: the security token service can't let the caller impersonate %s now (HTTP 503): try again later", serviceAccount)
	case granted.Error == "invalid_grant":
		return fmt.Errorf("origincloud: the security token service refused to let the caller impersonate %s: the caller must be a person (a service account's token can't impersonate another) holding iam.service_account.get_access_token on it (roles/iam.service-account-token-creator), and the account must exist and be active; the account's audit log records why", serviceAccount)
	case granted.Error == "invalid_request":
		return fmt.Errorf("origincloud: the security token service refused the impersonation of %s as malformed: %s", serviceAccount, granted.reason())
	default:
		return fmt.Errorf("origincloud: the security token service refused the impersonation of %s (HTTP %d): %s", serviceAccount, status, granted.reason())
	}
}
