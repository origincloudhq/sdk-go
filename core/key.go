package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	envCredentialsFile = "ORIGINCLOUD_CREDENTIALS_FILE"

	// The type a service account key's credentials file names.
	credentialsFileType = "service_account_key"

	// The JWT bearer grant (RFC 7523) a key's assertion is exchanged with.
	jwtBearerGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

	// How long an assertion is valid: long enough for a slow exchange,
	// well within the hour the security token service accepts.
	assertionLifetime = 5 * time.Minute
)

// ServiceAccountKeyCredentials exchange assertions signed with a service
// account's key, from the credentials file creating the key returned, for
// the account's access tokens at the security token service, as they
// expire. Concurrent callers share one exchange.
//
// They hold the private key: they never print it, whatever verb formats
// them.
type ServiceAccountKeyCredentials struct {
	// HTTPClient sends the exchanges; [http.DefaultClient] when nil.
	HTTPClient *http.Client

	key            string
	serviceAccount string
	endpoint       string
	privateKey     *ecdsa.PrivateKey
	cache          tokenCache
}

// The credentials file a service account key's creation returns.
type credentialsFile struct {
	Type           string `json:"type"`
	Name           string `json:"name"`
	ServiceAccount string `json:"service_account"`
	Algorithm      string `json:"algorithm"`
	PrivateKey     string `json:"private_key"`
	STSEndpoint    string `json:"sts_endpoint"`
}

// CredentialsFromFile returns the credentials of the service account key
// whose credentials file is at path: the file creating the key returned,
// saved as it is.
func CredentialsFromFile(path string) (*ServiceAccountKeyCredentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("origincloud: reading the credentials file: %w", err)
	}
	c, err := CredentialsFromJSON(data)
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, path)
	}
	return c, nil
}

// CredentialsFromJSON returns the credentials of the service account key
// whose credentials file data holds, such as one read from a secret
// manager.
func CredentialsFromJSON(data []byte) (*ServiceAccountKeyCredentials, error) {
	var file credentialsFile
	if err := json.Unmarshal(data, &file); err != nil {
		// The error of a file that isn't JSON could quote its contents.
		return nil, errors.New("origincloud: the credentials file is not JSON")
	}
	switch {
	case file.Type != credentialsFileType:
		return nil, fmt.Errorf("origincloud: the credentials file's type is %q, not %q: use the file creating a service account key returned", file.Type, credentialsFileType)
	case file.Algorithm != "ES256":
		return nil, fmt.Errorf("origincloud: the credentials file's algorithm is %q; this SDK signs with ES256 only", file.Algorithm)
	case file.Name == "" || file.ServiceAccount == "":
		return nil, errors.New("origincloud: the credentials file names no key or no service account")
	}
	endpoint, err := url.Parse(file.STSEndpoint)
	if err != nil || endpoint.Host == "" || !secureEndpoint(endpoint) {
		return nil, fmt.Errorf("origincloud: the credentials file's sts_endpoint %q is not an https URL", file.STSEndpoint)
	}
	privateKey, err := parseES256Key(file.PrivateKey)
	if err != nil {
		return nil, err
	}
	return &ServiceAccountKeyCredentials{
		key:            file.Name,
		serviceAccount: file.ServiceAccount,
		endpoint:       strings.TrimSuffix(file.STSEndpoint, "/"),
		privateKey:     privateKey,
	}, nil
}

// parseES256Key reads a PKCS #8 P-256 private key in PEM. Its errors never
// quote the key.
func parseES256Key(data string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(data))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("origincloud: the credentials file's private_key is not a PKCS #8 private key in PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("origincloud: the credentials file's private_key can't be read as a PKCS #8 private key")
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("origincloud: the credentials file's private_key is not a P-256 key")
	}
	return key, nil
}

// Key is the full name of the key the credentials sign with.
func (c *ServiceAccountKeyCredentials) Key() string { return c.key }

// ServiceAccount is the full name of the service account the credentials
// act as.
func (c *ServiceAccountKeyCredentials) ServiceAccount() string { return c.serviceAccount }

// String names the key, never its private half.
func (c *ServiceAccountKeyCredentials) String() string {
	return "ServiceAccountKeyCredentials{" + c.key + "}"
}

// GoString is String, so that %#v doesn't print the private key either.
func (c *ServiceAccountKeyCredentials) GoString() string { return c.String() }

// Format prints String for every verb, so that %+v doesn't print the
// private key.
func (c *ServiceAccountKeyCredentials) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(c.String()))
}

// Token returns the current access token, exchanging a fresh assertion for
// a new one about a minute before it expires.
func (c *ServiceAccountKeyCredentials) Token(ctx context.Context) (string, error) {
	return c.cache.getWithin(federatedExpiryMargin, func() (string, time.Time, error) {
		if c.privateKey == nil {
			return "", time.Time{}, errors.New("origincloud: service account key credentials need a credentials file: use CredentialsFromFile or CredentialsFromJSON")
		}
		assertion, err := c.assertion(time.Now())
		if err != nil {
			return "", time.Time{}, err
		}
		status, granted, err := exchangeAtSTS(ctx, c.HTTPClient, c.endpoint, url.Values{
			"grant_type": {jwtBearerGrant},
			"assertion":  {assertion},
		})
		if err != nil {
			return "", time.Time{}, err
		}
		if err := keyRefusal(status, granted, c.key); err != nil {
			return "", time.Time{}, err
		}
		return granted.AccessToken, granted.expiry(), nil
	})
}

// assertion is a JWT (RFC 7523) signed ES256 with the key, issued now, for
// the security token service.
func (c *ServiceAccountKeyCredentials) assertion(now time.Time) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", fmt.Errorf("origincloud: making an assertion: %w", err)
	}
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": c.key})
	if err != nil {
		return "", fmt.Errorf("origincloud: making an assertion: %w", err)
	}
	claims, err := json.Marshal(map[string]any{
		"iss": c.serviceAccount,
		"sub": c.serviceAccount,
		"aud": c.endpoint,
		"iat": now.Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
		"jti": hex.EncodeToString(jti),
	})
	if err != nil {
		return "", fmt.Errorf("origincloud: making an assertion: %w", err)
	}
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, c.privateKey, digest[:])
	if err != nil {
		return "", fmt.Errorf("origincloud: signing an assertion: %w", err)
	}
	// JWS's ES256 signature is r and s, 32 bytes each (RFC 7518, 3.4).
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// keyRefusal is why the security token service handed out no access token
// for a key's assertion, or nil when it handed one out.
func keyRefusal(status int, granted grantedToken, key string) error {
	switch {
	case status == http.StatusOK && granted.AccessToken != "":
		return nil
	case status == http.StatusOK:
		return errors.New("origincloud: the security token service's answer is not a token")
	case status == http.StatusServiceUnavailable:
		return errors.New("origincloud: the security token service can't check the service account key's assertion now (HTTP 503): try again later")
	case granted.Error == "invalid_grant":
		return fmt.Errorf("origincloud: the security token service refused the assertion of the key %s: the key may be deleted, the account deleted or not an EXTERNAL one, or this machine's clock wrong; the audit log records why", key)
	default:
		return fmt.Errorf("origincloud: the security token service refused the assertion (HTTP %d): %s", status, granted.reason())
	}
}

// secureEndpoint reports whether an assertion may be sent to endpoint: over
// https, or over http to the loopback interface, as a local test server
// answers. An assertion sent in the clear could be replayed until it expires.
func secureEndpoint(endpoint *url.URL) bool {
	switch endpoint.Scheme {
	case "https":
		return true
	case "http":
		host := endpoint.Hostname()
		if host == "localhost" {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	}
	return false
}
