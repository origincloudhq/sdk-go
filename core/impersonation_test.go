package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

const impersonatedAccount = "//iam.origincloudapis.eu/organizations/o/serviceAccounts/deployer"

// personTokens hands out a person's access tokens numbered from 1, a new
// one at every call, as a source whose tokens are renewed.
type personTokens struct{ n atomic.Int32 }

func (p *personTokens) Token(context.Context) (string, error) {
	return fmt.Sprintf("person-%d", p.n.Add(1)), nil
}

// failingSource has no token, for the reason err.
type failingSource struct{ err error }

func (f failingSource) Token(context.Context) (string, error) { return "", f.err }

func impersonating(t *testing.T, source Credentials, sts *stsServer) *ImpersonatedCredentials {
	t.Helper()
	credentials, err := Impersonate(source, impersonatedAccount)
	if err != nil {
		t.Fatal(err)
	}
	credentials.Endpoint = sts.URL
	return credentials
}

// apiServer is a fake API that records the Authorization header of every
// call.
type apiServer struct {
	*httptest.Server

	mu   sync.Mutex
	auth []string
}

func newAPIServer(t *testing.T) *apiServer {
	t.Helper()
	a := &apiServer{}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *apiServer) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.auth...)
}

func call(client *Client) error {
	return client.Service("pvs", true).Call(context.Background(), Request{
		Method: http.MethodGet, Path: "/v1beta1/projects/p/zones/z/volumes",
	}, &structpb.Struct{})
}

// impersonation returns the exchange that named a service account, and
// the token the STS answered it with.
func impersonation(t *testing.T, sts *stsServer) (url.Values, string) {
	t.Helper()
	var found url.Values
	token := ""
	for i, form := range sts.exchanges() {
		if form.Get("service_account") == "" {
			continue
		}
		if found != nil {
			t.Fatalf("exchanges = %v, want one impersonation", sts.exchanges())
		}
		found, token = form, fmt.Sprintf("access-%d", i+1)
	}
	if found == nil {
		t.Fatalf("exchanges = %v, want an impersonation", sts.exchanges())
	}
	return found, token
}

func TestImpersonationExchangesTheSourcesTokenForTheAccounts(t *testing.T) {
	sts := newSTSServer(t)
	sts.expiresIn = 300
	credentials := impersonating(t, AccessToken("person-token"), sts)

	for range 3 {
		if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
			t.Fatalf("Token() = %q, %v; want access-1", token, err)
		}
	}
	// RFC 8693 with the account's full name, as the STS takes it: no
	// audience, as the token is for the API.
	want := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {"person-token"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"service_account":    {impersonatedAccount},
	}
	if got := sts.exchanges(); len(got) != 1 || got[0].Encode() != want.Encode() {
		t.Fatalf("exchanges = %v, want %v once", got, want)
	}
	if credentials.ServiceAccount() != impersonatedAccount {
		t.Fatalf("ServiceAccount() = %q", credentials.ServiceAccount())
	}
}

func TestTheAccountsTokenIsRenewedBeforeItExpiresWithAFreshSourceToken(t *testing.T) {
	sts := newSTSServer(t)
	// Renewed halfway through so short a lifetime: after half a second.
	sts.expiresIn = 1
	credentials := impersonating(t, &personTokens{}, sts)

	for _, want := range []string{"access-1", "access-1"} {
		if token, err := credentials.Token(context.Background()); err != nil || token != want {
			t.Fatalf("Token() = %q, %v; want %s", token, err, want)
		}
	}
	// access-1 is still valid for another 400ms, and is replaced.
	time.Sleep(600 * time.Millisecond)
	if token, err := credentials.Token(context.Background()); err != nil || token != "access-2" {
		t.Fatalf("Token() = %q, %v; want access-2", token, err)
	}
	if got := strings.Join(sts.subjects(), ", "); got != "person-1, person-2" {
		t.Fatalf("subjects = %s, want a fresh source token at each exchange", got)
	}
}

func TestTheAccountsTokenIsRenewedAMinuteBeforeItExpires(t *testing.T) {
	issued := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		lifetime, renewedAfter time.Duration
	}{
		{5 * time.Minute, 4 * time.Minute},
		{2 * time.Minute, time.Minute},
		// Capped by a source token about to expire: halfway through.
		{50 * time.Second, 25 * time.Second},
		{0, 0},
	} {
		if got := renewalTime(issued, c.lifetime).Sub(issued); got != c.renewedAfter {
			t.Errorf("a token living %s is renewed after %s, want %s", c.lifetime, got, c.renewedAfter)
		}
	}
}

func TestConcurrentCallersShareOneImpersonation(t *testing.T) {
	sts := newSTSServer(t)
	credentials := impersonating(t, &personTokens{}, sts)

	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
				t.Errorf("Token() = %q, %v; want access-1", token, err)
			}
		})
	}
	wg.Wait()
	if got := len(sts.exchanges()); got != 1 {
		t.Fatalf("%d exchanges, want one", got)
	}
}

func TestARefusedImpersonationIsAnErrorNamingTheAccount(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		says   string
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant","error_description":"the exchange was refused"}`, "roles/iam.service-account-token-creator"},
		{http.StatusBadRequest, `{"error":"invalid_request","error_description":"no service_account to impersonate"}`, "malformed: no service_account to impersonate"},
		{http.StatusBadRequest, `{"error":"invalid_target"}`, "HTTP 400"},
		{http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`, "try again later"},
	} {
		t.Run(c.says, func(t *testing.T) {
			sts := newSTSServer(t)
			sts.answer = func() (int, string) { return c.status, c.body }
			credentials := impersonating(t, AccessToken("person-token"), sts)

			token, err := credentials.Token(context.Background())
			if err == nil || token != "" {
				t.Fatalf("Token() = %q, %v; want an error and no token", token, err)
			}
			if !strings.Contains(err.Error(), impersonatedAccount) || !strings.Contains(err.Error(), c.says) {
				t.Fatalf("error = %v, want it to name %s and say %q", err, impersonatedAccount, c.says)
			}
			assertNoToken(t, err, "person-token")
		})
	}
}

func TestAnUnavailableSTSIsAskedThreeTimesToImpersonate(t *testing.T) {
	sts := newSTSServer(t)
	var answered atomic.Int32
	sts.answer = func() (int, string) {
		if answered.Add(1) < 3 {
			return http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`
		}
		return 0, ""
	}
	credentials := impersonating(t, AccessToken("person-token"), sts)

	if token, err := credentials.Token(context.Background()); err != nil || token != "access-3" {
		t.Fatalf("Token() = %q, %v; want the third answer's access-3", token, err)
	}
}

func TestASourceWithoutATokenIsAnErrorNamingTheAccount(t *testing.T) {
	sts := newSTSServer(t)
	credentials := impersonating(t, failingSource{ErrNoCredentials}, sts)

	_, err := credentials.Token(context.Background())
	if !errors.Is(err, ErrNoCredentials) || !strings.Contains(err.Error(), impersonatedAccount) {
		t.Fatalf("error = %v, want ErrNoCredentials naming the account", err)
	}
	if got := len(sts.exchanges()); got != 0 {
		t.Fatalf("%d exchanges, want none", got)
	}
}

func TestOnlyAServiceAccountsFullNameIsImpersonated(t *testing.T) {
	for _, name := range []string{
		"",
		"organizations/o/serviceAccounts/deployer",
		"//iam.origincloudapis.eu/users/u",
		"//iam.origincloudapis.eu/organizations/o",
		"//pvs.origincloudapis.eu/organizations/o/serviceAccounts/deployer",
		"urn:origincloud:iam:organizations/o/serviceAccounts/deployer",
	} {
		if _, err := Impersonate(AccessToken("person-token"), name); err == nil {
			t.Errorf("Impersonate(%q) succeeded, want it refused", name)
		}
	}
	if _, err := Impersonate(nil, impersonatedAccount); err == nil {
		t.Error("Impersonate(nil, …) succeeded, want it refused")
	}
}

func TestTheSourcesTokenIsNeverSentInTheClear(t *testing.T) {
	source := &personTokens{}
	credentials, err := Impersonate(source, impersonatedAccount)
	if err != nil {
		t.Fatal(err)
	}
	credentials.Endpoint = "http://sts.example.test"

	if _, err := credentials.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "not an https URL") {
		t.Fatalf("Token() error = %v, want the endpoint refused", err)
	}
	if source.n.Load() != 0 {
		t.Fatal("the source was asked for a token")
	}
}

func TestTheClientImpersonatesTheAccountTheEnvironmentNames(t *testing.T) {
	dir := clearCredentialEnv(t)
	// A person's login.
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
	sts, api := newSTSServer(t), newAPIServer(t)
	t.Setenv(envImpersonateServiceAccount, impersonatedAccount)
	t.Setenv(envSTSEndpoint, sts.URL)

	client, err := NewClient(context.Background(), WithEndpoint(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := call(client); err != nil {
			t.Fatal(err)
		}
	}
	if got := api.calls(); strings.Join(got, ", ") != "Bearer access-1, Bearer access-1" {
		t.Fatalf("Authorization = %v, want the account's token", got)
	}
	if form, _ := impersonation(t, sts); form.Get("subject_token") != "stored-access" || form.Get("service_account") != impersonatedAccount {
		t.Fatalf("exchange = %v, want the CLI session's token for %s", form, impersonatedAccount)
	}
}

func TestTheOptionOverridesTheEnvironment(t *testing.T) {
	const other = "//iam.origincloudapis.eu/organizations/o/serviceAccounts/reader"
	clearCredentialEnv(t)
	sts, api := newSTSServer(t), newAPIServer(t)
	t.Setenv(envImpersonateServiceAccount, impersonatedAccount)
	t.Setenv(envSTSEndpoint, sts.URL)

	client, err := NewClient(context.Background(), WithEndpoint(api.URL),
		WithCredentials(AccessToken("person-token")), WithImpersonateServiceAccount(other))
	if err != nil {
		t.Fatal(err)
	}
	if err := call(client); err != nil {
		t.Fatal(err)
	}
	if form, _ := impersonation(t, sts); form.Get("service_account") != other || form.Get("subject_token") != "person-token" {
		t.Fatalf("exchange = %v, want the given credentials' token for %s", form, other)
	}

	// An empty one impersonates no one.
	client, err = NewClient(context.Background(), WithEndpoint(api.URL),
		WithCredentials(AccessToken("person-token")), WithImpersonateServiceAccount(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := call(client); err != nil {
		t.Fatal(err)
	}
	if got := api.calls(); len(got) != 2 || got[1] != "Bearer person-token" {
		t.Fatalf("Authorization = %v, want the person's own token without impersonation", got)
	}
}

func TestAClientImpersonatingAnAccountThatIsNoneIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envImpersonateServiceAccount, "organizations/o/serviceAccounts/deployer")

	_, err := NewClient(context.Background(), WithCredentials(AccessToken("person-token")))
	if err == nil || !strings.Contains(err.Error(), "service account to impersonate") {
		t.Fatalf("NewClient() error = %v, want the name refused", err)
	}
}

func TestARefusedImpersonationSendsNoCall(t *testing.T) {
	clearCredentialEnv(t)
	sts, api := newSTSServer(t), newAPIServer(t)
	sts.answer = func() (int, string) { return http.StatusBadRequest, `{"error":"invalid_grant"}` }
	t.Setenv(envSTSEndpoint, sts.URL)

	client, err := NewClient(context.Background(), WithEndpoint(api.URL),
		WithCredentials(AccessToken("person-token")), WithImpersonateServiceAccount(impersonatedAccount))
	if err != nil {
		t.Fatal(err)
	}
	err = call(client)
	if err == nil || !strings.Contains(err.Error(), impersonatedAccount) {
		t.Fatalf("call error = %v, want the refusal naming the account", err)
	}
	if got := api.calls(); len(got) != 0 {
		t.Fatalf("Authorization = %v, want no call: never the caller's own token", got)
	}
}

func TestImpersonationWorksOnTopOfEveryCredentialInTheChain(t *testing.T) {
	for _, c := range []struct {
		name string
		// set puts the credential in the environment, with the STS at
		// sts, and returns the token it gives, or "" when it can't be
		// known in advance.
		set func(t *testing.T, dir string, sts *stsServer) string
	}{
		{"an access token", func(t *testing.T, _ string, _ *stsServer) string {
			t.Setenv(envAccessToken, "person-token")
			return "person-token"
		}},
		{"a credentials file", func(t *testing.T, _ string, _ *stsServer) string {
			path, _ := newKeyFile(t)
			t.Setenv(envCredentialsFile, path)
			return ""
		}},
		{"a CI job's OIDC token", func(t *testing.T, _ string, _ *stsServer) string {
			t.Setenv(envOIDCToken, unsignedJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()}))
			// The federated exchange comes first, at the same STS.
			return "access-1"
		}},
		{"a person's login", func(t *testing.T, dir string, _ *stsServer) string {
			writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))
			return "stored-access"
		}},
		{"the instance's metadata service", func(t *testing.T, _ string, _ *stsServer) string {
			t.Setenv(envMetadataEndpoint, newMetadataServer(t).URL)
			return "access-1"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := clearCredentialEnv(t)
			sts, api := newSTSServer(t), newAPIServer(t)
			t.Setenv(envSTSEndpoint, sts.URL)
			t.Setenv(envImpersonateServiceAccount, impersonatedAccount)
			subject := c.set(t, dir, sts)

			client, err := NewClient(context.Background(), WithEndpoint(api.URL))
			if err != nil {
				t.Fatal(err)
			}
			if err := call(client); err != nil {
				t.Fatal(err)
			}
			form, token := impersonation(t, sts)
			if got := form.Get("subject_token"); got == "" || subject != "" && got != subject {
				t.Fatalf("subject_token = %q, want %q", got, subject)
			}
			if got := api.calls(); len(got) != 1 || got[0] != "Bearer "+token {
				t.Fatalf("Authorization = %v, want the account's %s", got, token)
			}
		})
	}
}
