package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const gitHubRequestToken = "github-request-token"

// gitHubServer is a fake GitHub Actions OIDC token endpoint. It hands out
// job tokens numbered from 1 to requests that carry gitHubRequestToken, and
// answers status instead when it is not 0.
type gitHubServer struct {
	*httptest.Server

	mu      sync.Mutex
	queries []url.Values
	auth    []string
	status  int
}

func newGitHubServer(t *testing.T) *gitHubServer {
	t.Helper()
	g := &gitHubServer{}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.queries = append(g.queries, r.URL.Query())
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		if g.status != 0 {
			w.WriteHeader(g.status)
			_, _ = w.Write([]byte(`{"message":"Unable to get ACTIONS_ID_TOKEN_REQUEST_URL env variable"}`))
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/idtoken" || r.Header.Get("Authorization") != "bearer "+gitHubRequestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"value": fmt.Sprintf("job-token-%d", len(g.queries))})
	}))
	t.Cleanup(g.Close)
	return g
}

func (g *gitHubServer) requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.queries)
}

// request returns the query and the Authorization header of the i-th
// request.
func (g *gitHubServer) request(i int) (url.Values, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.queries[i], g.auth[i]
}

// stsServer is a fake security token service. It hands out access tokens
// numbered from 1, expiring after expiresIn seconds, and records the
// subject tokens it got; answer, when it returns a status, answers that
// with its body instead.
type stsServer struct {
	*httptest.Server

	mu        sync.Mutex
	forms     []url.Values
	expiresIn int
	answer    func() (int, string)
}

func newSTSServer(t *testing.T) *stsServer {
	t.Helper()
	s := &stsServer{expiresIn: 900, answer: func() (int, string) { return 0, "" }}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing the exchange: %v", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.forms = append(s.forms, r.PostForm)
		if status, body := s.answer(); status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":      fmt.Sprintf("access-%d", len(s.forms)),
			"issued_token_type": "urn:ietf:params:oauth:token-type:access_token",
			"token_type":        "Bearer",
			"expires_in":        s.expiresIn,
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stsServer) exchanges() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.forms...)
}

func (s *stsServer) subjects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var subjects []string
	for _, form := range s.forms {
		subjects = append(subjects, form.Get("subject_token"))
	}
	return subjects
}

// assertNoToken fails the test when err names one of tokens.
func assertNoToken(t *testing.T, err error, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("the error names a token: %v", err)
		}
	}
}

func TestAGitHubActionsJobsTokenIsExchanged(t *testing.T) {
	github, sts := newGitHubServer(t), newSTSServer(t)
	credentials := FederatedFromGitHubActions(github.URL+"/idtoken?api-version=2.0", gitHubRequestToken)
	credentials.Endpoint = sts.URL + "/"

	for range 3 {
		if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
			t.Fatalf("Token() = %q, %v; want access-1", token, err)
		}
	}
	if github.requests() != 1 || len(sts.exchanges()) != 1 {
		t.Fatalf("%d job tokens and %d exchanges, want the first access token reused", github.requests(), len(sts.exchanges()))
	}
	query, auth := github.request(0)
	if query.Get("api-version") != "2.0" || query.Get("audience") != OIDCAudience || len(query) != 2 {
		t.Fatalf("job token request query = %v, want GitHub's with the audience added", query)
	}
	if auth != "bearer "+gitHubRequestToken {
		t.Fatalf("Authorization = %q", auth)
	}
	want := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {"job-token-1"},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	if got := sts.exchanges()[0]; got.Encode() != want.Encode() {
		t.Fatalf("exchange = %v, want %v", got, want)
	}
}

func TestTheGitHubTokenRequestGetsAQueryWhenItHasNone(t *testing.T) {
	github, sts := newGitHubServer(t), newSTSServer(t)
	credentials := FederatedFromGitHubActions(github.URL+"/idtoken", gitHubRequestToken)
	credentials.Endpoint = sts.URL
	credentials.Audience = "https://example.test/aud"

	if _, err := credentials.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if query, _ := github.request(0); query.Get("audience") != "https://example.test/aud" {
		t.Fatalf("audience = %q, want the one set", query.Get("audience"))
	}
}

func TestAnExchangedTokenIsRenewedWithAFreshJobToken(t *testing.T) {
	github, sts := newGitHubServer(t), newSTSServer(t)
	// Within a minute of its expiry from the start.
	sts.expiresIn = 50
	credentials := FederatedFromGitHubActions(github.URL+"/idtoken?api-version=2.0", gitHubRequestToken)
	credentials.Endpoint = sts.URL

	for _, want := range []string{"access-1", "access-2"} {
		if token, err := credentials.Token(context.Background()); err != nil || token != want {
			t.Fatalf("Token() = %q, %v; want %s", token, err, want)
		}
	}
	if got := strings.Join(sts.subjects(), ", "); got != "job-token-1, job-token-2" {
		t.Fatalf("exchanged %s, want a fresh job token each time", got)
	}
}

func TestATokenFileIsReadAgainAtEveryExchange(t *testing.T) {
	sts := newSTSServer(t)
	sts.expiresIn = 50
	path := filepath.Join(t.TempDir(), "token")
	credentials := FederatedFromFile(path)
	credentials.Endpoint = sts.URL

	for _, subject := range []string{"file-token-1", "file-token-2"} {
		if err := os.WriteFile(path, []byte(subject+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := credentials.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(sts.subjects(), ", "); got != "file-token-1, file-token-2" {
		t.Fatalf("exchanged %s, want the file as it is at each exchange", got)
	}

	// A file that is gone is an error, not a token.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := credentials.Token(context.Background())
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want the file missing", err)
	}
}

func TestConcurrentCallersShareOneExchange(t *testing.T) {
	github, sts := newGitHubServer(t), newSTSServer(t)
	credentials := FederatedFromGitHubActions(github.URL+"/idtoken?api-version=2.0", gitHubRequestToken)
	credentials.Endpoint = sts.URL

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
				t.Errorf("Token() = %q, %v; want access-1", token, err)
			}
		})
	}
	wg.Wait()
	if github.requests() != 1 || len(sts.exchanges()) != 1 {
		t.Fatalf("%d job tokens and %d exchanges, want one", github.requests(), len(sts.exchanges()))
	}
}

func TestAnUnavailableSTSIsAskedThreeTimes(t *testing.T) {
	sts := newSTSServer(t)
	var unavailable atomic.Int32
	sts.answer = func() (int, string) {
		if unavailable.Add(-1) >= 0 {
			return http.StatusServiceUnavailable, `{"error":"temporarily_unavailable"}`
		}
		return 0, ""
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Unavailable twice: the third attempt succeeds, after waiting 200 ms
	// and 400 ms.
	unavailable.Store(2)
	start := time.Now()
	credentials := FederatedFromFile(path)
	credentials.Endpoint = sts.URL
	if token, err := credentials.Token(context.Background()); err != nil || token != "access-3" {
		t.Fatalf("Token() = %q, %v; want access-3", token, err)
	}
	if took := time.Since(start); took < 600*time.Millisecond {
		t.Fatalf("three attempts took %v, want the waits between them", took)
	}

	// Unavailable three times: the error says it is passing.
	unavailable.Store(3)
	again := FederatedFromFile(path)
	again.Endpoint = sts.URL
	_, err := again.Token(context.Background())
	if !errors.Is(err, errSTSUnavailable) {
		t.Fatalf("err = %v, want the service unavailable", err)
	}
	if left := unavailable.Load(); left != 0 {
		t.Fatalf("%d attempts left unmade", left)
	}
	assertNoToken(t, err, "file-token")
}

func TestWaitingForTheSTSEndsWithTheCallersContext(t *testing.T) {
	sts := newSTSServer(t)
	sts.answer = func() (int, string) { return http.StatusServiceUnavailable, "" }
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	credentials := FederatedFromFile(path)
	credentials.Endpoint = sts.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := credentials.Token(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
}

func TestTheSTSSaysWhyItRefusesAJobToken(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, "origincloud: the security token service refused the CI job's OIDC token: check the federated credential's issuer, audience and conditions against the token's claims"},
		{http.StatusBadRequest, `{"error":"invalid_request","error_description":"subject_token_type must be a JWT"}`, "origincloud: the security token service refused the exchange as malformed: subject_token_type must be a JWT"},
		{http.StatusInternalServerError, `{"error":"server_error"}`, "origincloud: the security token service refused the exchange (HTTP 500): server_error"},
	} {
		github, sts := newGitHubServer(t), newSTSServer(t)
		sts.answer = func() (int, string) { return tc.status, tc.body }
		credentials := FederatedFromGitHubActions(github.URL+"/idtoken?api-version=2.0", gitHubRequestToken)
		credentials.Endpoint = sts.URL

		_, err := credentials.Token(context.Background())
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: err = %v, want %q", tc.body, err, tc.want)
			continue
		}
		assertNoToken(t, err, "job-token", gitHubRequestToken)
		if len(sts.exchanges()) != 1 {
			t.Errorf("%s: exchanged %d times, want once", tc.body, len(sts.exchanges()))
		}
	}
}

func TestAFailedGitHubTokenRequestNamesThePermission(t *testing.T) {
	github, sts := newGitHubServer(t), newSTSServer(t)
	github.status = http.StatusForbidden
	credentials := FederatedFromGitHubActions(github.URL+"/idtoken?api-version=2.0", gitHubRequestToken)
	credentials.Endpoint = sts.URL

	_, err := credentials.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "`permissions: id-token: write`") || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v, want the permission named", err)
	}
	assertNoToken(t, err, gitHubRequestToken)
	if len(sts.exchanges()) != 0 {
		t.Fatal("the security token service was asked without a job token")
	}
}

func TestAGitHubActionsJobUsesItsTokenByDefault(t *testing.T) {
	dir := clearCredentialEnv(t)
	// Federation comes before the CLI's session.
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	github, sts := newGitHubServer(t), newSTSServer(t)
	t.Setenv(envGitHubTokenURL, github.URL+"/idtoken?api-version=2.0")
	t.Setenv(envGitHubRequestToken, gitHubRequestToken)
	t.Setenv(envSTSEndpoint, sts.URL)
	t.Setenv(envOIDCAudience, "https://example.test/aud")

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
		t.Fatalf("Token() = %q, %v; want the exchanged access-1", token, err)
	}
	if query, _ := github.request(0); query.Get("audience") != "https://example.test/aud" {
		t.Fatalf("audience = %q, want the environment's", query.Get("audience"))
	}
}

func TestATokenFileComesBeforeTheGitHubActionsJobsToken(t *testing.T) {
	clearCredentialEnv(t)
	github, sts := newGitHubServer(t), newSTSServer(t)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envGitHubTokenURL, github.URL+"/idtoken?api-version=2.0")
	t.Setenv(envGitHubRequestToken, gitHubRequestToken)
	t.Setenv(envOIDCTokenFile, path)
	t.Setenv(envSTSEndpoint, sts.URL)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sts.subjects(), ", "); got != "file-token" || github.requests() != 0 {
		t.Fatalf("exchanged %q and asked GitHub %d times, want the file's token alone", got, github.requests())
	}
}

func TestAServiceAccountInTheEnvironmentComesBeforeAJobToken(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envGitHubTokenURL, "https://github.test/idtoken?api-version=2.0")
	t.Setenv(envGitHubRequestToken, gitHubRequestToken)
	t.Setenv(envClientID, "id")
	t.Setenv(envClientSecret, "secret")

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials.(*ServiceAccountCredentials); !ok {
		t.Fatalf("credentials = %T, want a service account's", credentials)
	}
}

// A variable that is set but blank is still the credential, as in the
// other SDKs: the job meant to hand over a token, so it hears that it's
// empty instead of being sent to the next source.
func TestABlankVariableIsAnEmptyJobToken(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envOIDCToken, " \n")
	t.Setenv(envGitHubTokenURL, "https://github.test/idtoken?api-version=2.0")
	t.Setenv(envGitHubRequestToken, gitHubRequestToken)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("Token() error = %v, want the empty token's", err)
	}
}

func TestAJobTokenIsLeftOutWhenDisabledOrIncomplete(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"disabled": {
			envGitHubTokenURL:     "https://github.test/idtoken?api-version=2.0",
			envGitHubRequestToken: gitHubRequestToken,
			envOIDCTokenFile:      "/var/run/token",
			envOIDCToken:          "variable-token",
			envOIDCDisabled:       "True",
		},
		"disabled with the variable alone": {
			envOIDCToken:    "variable-token",
			envOIDCDisabled: "true",
		},
		"no request token": {envGitHubTokenURL: "https://github.test/idtoken?api-version=2.0"},
		"no request URL":   {envGitHubRequestToken: gitHubRequestToken},
	} {
		t.Run(name, func(t *testing.T) {
			clearCredentialEnv(t)
			for key, value := range env {
				t.Setenv(key, value)
			}

			credentials, err := DefaultCredentials(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := credentials.(*InstanceMetadataCredentials); !ok {
				t.Fatalf("credentials = %T, want the metadata service's", credentials)
			}
		})
	}
}

func TestADetectedJobTokenIsTheCredentialEvenWhenItFails(t *testing.T) {
	dir := clearCredentialEnv(t)
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	t.Setenv(envOIDCTokenFile, filepath.Join(dir, "missing"))

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want the token file's, not the CLI session's token", err)
	}
}

// unsignedJWT returns a JWT whose payload is claims, signed by no one: the
// SDK reads the claims without verifying them.
func unsignedJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"none"}`)) + "." + encode(payload) + ".signature"
}

func TestATokenInTheVariableIsExchanged(t *testing.T) {
	dir := clearCredentialEnv(t)
	// Federation comes before the CLI's session.
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	sts := newSTSServer(t)
	token := unsignedJWT(t, map[string]any{"sub": "project_path:group/project", "exp": time.Now().Add(time.Hour).Unix()})
	t.Setenv(envOIDCToken, token)
	t.Setenv(envSTSEndpoint, sts.URL)
	// The token was issued for its audience already: this one is GitHub's
	// alone.
	t.Setenv(envOIDCAudience, "https://example.test/aud")

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := credentials.Token(context.Background()); err != nil || got != "access-1" {
		t.Fatalf("Token() = %q, %v; want the exchanged access-1", got, err)
	}
	want := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {token},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:jwt"},
	}
	if got := sts.exchanges(); len(got) != 1 || got[0].Encode() != want.Encode() {
		t.Fatalf("exchanges = %v, want the variable's token once", got)
	}
}

func TestATokenFileComesBeforeTheVariable(t *testing.T) {
	clearCredentialEnv(t)
	sts := newSTSServer(t)
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("file-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envOIDCTokenFile, path)
	t.Setenv(envOIDCToken, "variable-token")
	t.Setenv(envSTSEndpoint, sts.URL)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sts.subjects(), ", "); got != "file-token" {
		t.Fatalf("exchanged %q, want the file's token alone", got)
	}
}

func TestTheVariableComesBeforeTheGitHubActionsJobsToken(t *testing.T) {
	clearCredentialEnv(t)
	github, sts := newGitHubServer(t), newSTSServer(t)
	t.Setenv(envGitHubTokenURL, github.URL+"/idtoken?api-version=2.0")
	t.Setenv(envGitHubRequestToken, gitHubRequestToken)
	t.Setenv(envOIDCToken, "variable-token")
	t.Setenv(envSTSEndpoint, sts.URL)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sts.subjects(), ", "); got != "variable-token" || github.requests() != 0 {
		t.Fatalf("exchanged %q and asked GitHub %d times, want the variable's token alone", got, github.requests())
	}
}

func TestAnExpiredTokenInTheVariableIsNotExchanged(t *testing.T) {
	clearCredentialEnv(t)
	sts := newSTSServer(t)
	expiry := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	token := unsignedJWT(t, map[string]any{"sub": "project_path:group/project", "exp": expiry.Unix()})
	t.Setenv(envOIDCToken, token)
	t.Setenv(envSTSEndpoint, sts.URL)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = credentials.Token(context.Background())
	want := "origincloud: the CI job's OIDC token in ORIGINCLOUD_OIDC_TOKEN expired at 2026-01-02T03:04:05Z, and a token in a variable can't be renewed: ask the CI provider for a longer-lived one, or use ORIGINCLOUD_OIDC_TOKEN_FILE"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	assertNoToken(t, err, token, strings.Split(token, ".")[1])
	if len(sts.exchanges()) != 0 {
		t.Fatal("the security token service was asked with an expired token")
	}
}

func TestAnExpiredTokenHandedOverNamesNoVariable(t *testing.T) {
	sts := newSTSServer(t)
	token := unsignedJWT(t, map[string]any{"exp": time.Now().Add(-time.Minute).Unix()})
	credentials := FederatedFromToken(token)
	credentials.Endpoint = sts.URL

	_, err := credentials.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "expired at") || strings.Contains(err.Error(), "ORIGINCLOUD_") {
		t.Fatalf("err = %v, want the expiry without a variable named", err)
	}
	assertNoToken(t, err, token, strings.Split(token, ".")[1])
	if len(sts.exchanges()) != 0 {
		t.Fatal("the security token service was asked with an expired token")
	}
}

func TestATokenWhosePayloadCantBeReadIsLeftToTheSTS(t *testing.T) {
	for name, token := range map[string]string{
		"not a JWT":          "opaque-token",
		"payload not JSON":   "header." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".signature",
		"payload not base64": "header.!!!.signature",
		"no exp":             unsignedJWT(t, map[string]any{"sub": "project_path:group/project"}),
		"exp not a number":   unsignedJWT(t, map[string]any{"exp": "yesterday"}),
	} {
		t.Run(name, func(t *testing.T) {
			sts := newSTSServer(t)
			credentials := FederatedFromToken(token)
			credentials.Endpoint = sts.URL

			if got, err := credentials.Token(context.Background()); err != nil || got != "access-1" {
				t.Fatalf("Token() = %q, %v; want access-1", got, err)
			}
			if got := sts.subjects(); len(got) != 1 || got[0] != token {
				t.Fatalf("exchanged %v, want the token as it is", got)
			}
		})
	}
}

func TestATokenHandedOverIsExchangedAgainAtEveryRenewal(t *testing.T) {
	sts := newSTSServer(t)
	// Within a minute of its expiry from the start.
	sts.expiresIn = 50
	token := unsignedJWT(t, map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	credentials := FederatedFromToken(token)
	credentials.Endpoint = sts.URL

	for _, want := range []string{"access-1", "access-2", "access-3"} {
		if got, err := credentials.Token(context.Background()); err != nil || got != want {
			t.Fatalf("Token() = %q, %v; want %s", got, err, want)
		}
	}
	if got := sts.subjects(); len(got) != 3 || got[0] != token || got[1] != token || got[2] != token {
		t.Fatalf("exchanged %v, want the same token each time", got)
	}
}

func TestAnEmptyTokenHandedOverIsNotExchanged(t *testing.T) {
	sts := newSTSServer(t)
	credentials := FederatedFromToken("  ")
	credentials.Endpoint = sts.URL

	if _, err := credentials.Token(context.Background()); err == nil {
		t.Fatal("an empty token was taken")
	}
	if len(sts.exchanges()) != 0 {
		t.Fatal("the security token service was asked without a token")
	}
}
