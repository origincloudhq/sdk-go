package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// clearCredentialEnv leaves the test with no credentials in the
// environment and a data directory of its own.
func clearCredentialEnv(t *testing.T) string {
	t.Helper()
	for _, name := range []string{envAccessToken, envClientID, envClientSecret, envRegion} {
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

	_, err := NewClient(context.Background())
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("err = %v, want ErrNoCredentials", err)
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
