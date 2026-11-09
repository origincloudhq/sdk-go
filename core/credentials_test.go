package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

// clearCredentialEnv leaves the test with no credentials in the
// environment and a data directory of its own.
func clearCredentialEnv(t *testing.T) string {
	t.Helper()
	for _, name := range []string{envAccessToken, envClientID, envClientSecret, envMetadataDisabled, envRegion} {
		t.Setenv(name, "")
	}
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	return dir
}

// tokenServer answers token requests with access tokens numbered from 1,
// expiring after expiresIn seconds, and records the last form it got.
func tokenServer(t *testing.T, expiresIn int) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var issued atomic.Int32
	var form atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing the token request: %v", err)
		}
		form.Store(r.PostForm)
		if r.PostForm.Get("client_secret") == "wrong" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client", "error_description": "Invalid client secret"})
			return
		}
		n := issued.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":       "token-" + string(rune('0'+n)),
			"expires_in":         expiresIn,
			"refresh_token":      "refresh-" + string(rune('0'+n)),
			"refresh_expires_in": 1800,
		})
	}))
	t.Cleanup(server.Close)
	return server, &issued, &form
}

func TestAnAccessTokenInTheEnvironmentComesFirst(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envAccessToken, "from-env")
	t.Setenv(envClientID, "id")
	t.Setenv(envClientSecret, "secret")

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token, _ := credentials.Token(context.Background()); token != "from-env" {
		t.Fatalf("token = %q, want the environment's", token)
	}
}

func TestAServiceAccountInTheEnvironmentComesBeforeTheCLISession(t *testing.T) {
	dir := clearCredentialEnv(t)
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
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

func TestHalfAServiceAccountIsRefused(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envClientID, "id")

	_, err := DefaultCredentials(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("err = %v, want the pair refused", err)
	}
}

func TestNoCredentialsAtAll(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envMetadataDisabled, "TRUE")

	_, err := NewClient(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

func TestWithNothingElseTheMetadataServiceComesLast(t *testing.T) {
	clearCredentialEnv(t)

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := credentials.(*InstanceMetadataCredentials); !ok {
		t.Fatalf("credentials = %T, want the metadata service's", credentials)
	}
}

func TestAServiceAccountTokenIsReusedUntilItIsAboutToExpire(t *testing.T) {
	server, issued, form := tokenServer(t, 300)
	credentials := &ServiceAccountCredentials{ClientID: "id", ClientSecret: "secret", TokenURL: server.URL}

	for range 3 {
		if token, err := credentials.Token(context.Background()); err != nil || token != "token-1" {
			t.Fatalf("Token() = %q, %v; want token-1", token, err)
		}
	}
	if n := issued.Load(); n != 1 {
		t.Fatalf("%d tokens requested, want 1", n)
	}
	got := form.Load().(url.Values)
	if got["grant_type"][0] != "client_credentials" || got["client_id"][0] != "id" || got["client_secret"][0] != "secret" {
		t.Fatalf("token request = %v", got)
	}

	// A token within the margin of its expiry is replaced.
	expiring := &ServiceAccountCredentials{ClientID: "id", ClientSecret: "secret"}
	tokenServerExpiry(t, expiring, 10)
	if token, _ := expiring.Token(context.Background()); token != "token-1" {
		t.Fatalf("token = %q", token)
	}
	if token, _ := expiring.Token(context.Background()); token != "token-2" {
		t.Fatalf("a token about to expire was reused: %q", token)
	}
}

// tokenServerExpiry points c at a token server whose tokens expire after
// expiresIn seconds.
func tokenServerExpiry(t *testing.T, c *ServiceAccountCredentials, expiresIn int) {
	server, _, _ := tokenServer(t, expiresIn)
	c.TokenURL = server.URL
}

func TestARefusedServiceAccountSaysWhy(t *testing.T) {
	server, _, _ := tokenServer(t, 300)
	credentials := &ServiceAccountCredentials{ClientID: "id", ClientSecret: "wrong", TokenURL: server.URL}

	_, err := credentials.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid client secret") {
		t.Fatalf("err = %v, want the identity provider's reason", err)
	}
}

func writeSession(t *testing.T, dir string, expires, refreshExpires time.Time) string {
	t.Helper()
	path := filepath.Join(dir, "origincloud", "session.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{
		"version":         1,
		"access_token":    "stored-access",
		"expires":         expires.UTC().Format(time.RFC3339),
		"refresh_token":   "stored-refresh",
		"refresh_expires": refreshExpires.UTC().Format(time.RFC3339),
		"token_type":      "Bearer",
	})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheCLISessionIsUsedAsStored(t *testing.T) {
	dir := clearCredentialEnv(t)
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(2*time.Hour))

	credentials, err := DefaultCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token, err := credentials.Token(context.Background()); err != nil || token != "stored-access" {
		t.Fatalf("Token() = %q, %v; want the stored access token", token, err)
	}
}

func TestAnExpiredCLISessionIsRenewedWithoutWritingItsFile(t *testing.T) {
	dir := clearCredentialEnv(t)
	path := writeSession(t, dir, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	before, _ := os.ReadFile(path)
	server, _, form := tokenServer(t, 300)

	credentials, err := CLISession()
	if err != nil {
		t.Fatal(err)
	}
	credentials.TokenURL = server.URL
	if token, err := credentials.Token(context.Background()); err != nil || token != "token-1" {
		t.Fatalf("Token() = %q, %v; want a renewed token", token, err)
	}
	got := form.Load().(url.Values)
	if got["grant_type"][0] != "refresh_token" || got["client_id"][0] != "origin-cli" || got["refresh_token"][0] != "stored-refresh" {
		t.Fatalf("renewal = %v", got)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("the CLI's session file was written")
	}
}

func TestACLISessionPastItsRefreshExpiryAsksToLogIn(t *testing.T) {
	dir := clearCredentialEnv(t)
	writeSession(t, dir, time.Now().Add(-time.Hour), time.Now().Add(-time.Minute))

	credentials, err := CLISession()
	if err != nil {
		t.Fatal(err)
	}
	_, err = credentials.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "origin id login") {
		t.Fatalf("err = %v, want a request to log in", err)
	}
}

// metadataServer is a fake metadata service. It opens sessions numbered
// from 1 and hands out access tokens numbered from 1, expiring in five
// minutes; credential answers with the status the credential request
// gets, or 0 to hand out a token.
type metadataServer struct {
	*httptest.Server

	mu         sync.Mutex
	requests   []string
	ttls       []string
	sessions   int
	tokens     int
	credential func(session string) int
}

func newMetadataServer(t *testing.T) *metadataServer {
	t.Helper()
	m := &metadataServer{credential: func(string) int { return 0 }}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.requests = append(m.requests, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			m.ttls = append(m.ttls, r.Header.Get("X-Origincloud-Metadata-Token-TTL-Seconds"))
			m.sessions++
			_, _ = w.Write([]byte("session-" + string(rune('0'+m.sessions))))
		case r.Method == http.MethodGet && r.URL.Path == "/latest/meta-data/service-account/token":
			if status := m.credential(r.Header.Get("X-Origincloud-Metadata-Token")); status != 0 {
				w.WriteHeader(status)
				return
			}
			m.tokens++
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token": "access-" + string(rune('0'+m.tokens)),
				"token_type":   "Bearer",
				"expires_at":   time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(m.Close)
	return m
}

func (m *metadataServer) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.requests...)
}

func TestTheMetadataServiceHandsOutTheInstancesToken(t *testing.T) {
	server := newMetadataServer(t)
	var sessions []string
	server.credential = func(session string) int {
		sessions = append(sessions, session)
		return 0
	}
	credentials := &InstanceMetadataCredentials{Endpoint: server.URL}

	for range 3 {
		if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
			t.Fatalf("Token() = %q, %v; want access-1", token, err)
		}
	}
	want := []string{"PUT /latest/api/token", "GET /latest/meta-data/service-account/token"}
	if got := server.seen(); strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	if server.ttls[0] != "21600" {
		t.Fatalf("session TTL = %q, want 21600", server.ttls[0])
	}
	if sessions[0] != "session-1" {
		t.Fatalf("the credential was asked for with session %q, want session-1", sessions[0])
	}
}

func TestAnUnknownMetadataSessionIsOpenedAgainOnce(t *testing.T) {
	server := newMetadataServer(t)
	server.credential = func(session string) int {
		if session == "session-1" {
			return http.StatusUnauthorized
		}
		return 0
	}
	credentials := &InstanceMetadataCredentials{Endpoint: server.URL}

	if token, err := credentials.Token(context.Background()); err != nil || token != "access-1" {
		t.Fatalf("Token() = %q, %v; want access-1", token, err)
	}
	if server.sessions != 2 {
		t.Fatalf("%d sessions opened, want 2", server.sessions)
	}

	// A session refused again is not opened a third time.
	server.credential = func(string) int { return http.StatusUnauthorized }
	refused := &InstanceMetadataCredentials{Endpoint: server.URL}
	_, err := refused.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "the metadata service answered HTTP 401") {
		t.Fatalf("err = %v, want the status", err)
	}
	if server.sessions != 4 {
		t.Fatalf("%d sessions opened, want 4", server.sessions)
	}
}

func TestTheMetadataServiceSaysWhyItHandsOutNoToken(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusNotFound:           "origincloud: the instance runs as no service account",
		http.StatusForbidden:          "origincloud: the metadata service refused a request that came through a proxy",
		http.StatusServiceUnavailable: "origincloud: the metadata service answered HTTP 503",
	} {
		server := newMetadataServer(t)
		server.credential = func(string) int { return status }
		credentials := &InstanceMetadataCredentials{Endpoint: server.URL}

		_, err := credentials.Token(context.Background())
		if err == nil || err.Error() != want || errors.Is(err, ErrNoCredentials) {
			t.Errorf("HTTP %d: err = %v, want %q", status, err, want)
		}
	}
}

func TestNoMetadataServiceIsNoCredentials(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + listener.Addr().String()
	listener.Close()
	credentials := &InstanceMetadataCredentials{Endpoint: closed}

	start := time.Now()
	_, err = credentials.Token(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("giving up took %v", took)
	}

	// Nor is an address the service knows no instance of.
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	_, err = (&InstanceMetadataCredentials{Endpoint: server.URL}).Token(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
	}
}

func TestTheMetadataServiceIsNeverAskedThroughAProxy(t *testing.T) {
	if transport := metadataHTTPClient.Transport.(*http.Transport); transport.Proxy != nil {
		t.Fatal("the metadata client goes through the environment's proxy")
	}
}
