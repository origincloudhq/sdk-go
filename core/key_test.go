package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
)

const (
	testKeyName        = "//iam.origincloudapis.eu/organizations/o/serviceAccounts/s/keys/k"
	testServiceAccount = "//iam.origincloudapis.eu/organizations/o/serviceAccounts/s"
)

// keySTS is a fake security token service that verifies a key's assertion
// as the real one does, against the public half of its key, and answers
// with access tokens numbered from 1.
type keySTS struct {
	*httptest.Server
	t         *testing.T
	public    *ecdsa.PublicKey
	expiresIn int
	issued    atomic.Int32

	mu    sync.Mutex
	jtis  map[string]bool
	claim []map[string]any
}

func newKeySTS(t *testing.T, public *ecdsa.PublicKey) *keySTS {
	t.Helper()
	s := &keySTS{t: t, public: public, expiresIn: 900, jtis: map[string]bool{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

func (s *keySTS) refuse(w http.ResponseWriter, why string) {
	s.t.Errorf("the STS refused the assertion: %s", why)
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
}

func (s *keySTS) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/token" || r.ParseForm() != nil {
		s.t.Errorf("request %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.PostForm.Get("grant_type") != jwtBearerGrant {
		s.refuse(w, "grant_type "+r.PostForm.Get("grant_type"))
		return
	}
	parts := strings.Split(r.PostForm.Get("assertion"), ".")
	if len(parts) != 3 {
		s.refuse(w, "not a JWT")
		return
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || len(signature) != 64 ||
		!ecdsa.Verify(s.public, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		s.refuse(w, "the signature doesn't verify")
		return
	}
	var header map[string]string
	var claims map[string]any
	if decodeSegment(parts[0], &header) != nil || decodeSegment(parts[1], &claims) != nil {
		s.refuse(w, "unreadable header or claims")
		return
	}
	if header["alg"] != "ES256" || header["kid"] != testKeyName {
		s.refuse(w, fmt.Sprintf("header %v", header))
		return
	}
	now := float64(time.Now().Unix())
	iat, _ := claims["iat"].(float64)
	exp, _ := claims["exp"].(float64)
	jti, _ := claims["jti"].(string)
	switch {
	case claims["iss"] != testServiceAccount || claims["sub"] != testServiceAccount:
		s.refuse(w, fmt.Sprintf("iss %v, sub %v", claims["iss"], claims["sub"]))
		return
	case claims["aud"] != s.URL:
		s.refuse(w, fmt.Sprintf("aud %v, want %s", claims["aud"], s.URL))
		return
	case iat > now+60 || exp <= now || exp-iat > 3600:
		s.refuse(w, fmt.Sprintf("iat %v, exp %v", iat, exp))
		return
	case jti == "" || len(jti) > 128:
		s.refuse(w, "jti "+jti)
		return
	}
	s.mu.Lock()
	reused := s.jtis[jti]
	s.jtis[jti] = true
	s.claim = append(s.claim, claims)
	s.mu.Unlock()
	if reused {
		s.refuse(w, "jti used before")
		return
	}
	n := s.issued.Add(1)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": fmt.Sprintf("sa-token-%d", n), "token_type": "Bearer", "expires_in": s.expiresIn,
	})
}

func decodeSegment(segment string, into any) error {
	data, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, into)
}

// newKeyFile writes the credentials file of a new key, whose assertions
// are exchanged at a fake STS, and returns its path and the STS.
func newKeyFile(t *testing.T) (string, *keySTS) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sts := newKeySTS(t, &private.PublicKey)
	path := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(path, credentialsJSON(t, private, sts.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, sts
}

func credentialsJSON(t *testing.T, private *ecdsa.PrivateKey, endpoint string) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{
		"type":            "service_account_key",
		"name":            testKeyName,
		"service_account": testServiceAccount,
		"algorithm":       "ES256",
		"private_key":     string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"sts_endpoint":    endpoint,
	})
	return data
}

func TestWithOnlyACredentialsFileTheAPIIsCalledAsTheServiceAccount(t *testing.T) {
	clearCredentialEnv(t)
	t.Setenv(envMetadataDisabled, "true")
	path, sts := newKeyFile(t)
	t.Setenv(envCredentialsFile, path)

	var auth []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = append(auth, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(api.Close)
	client, err := NewClient(context.Background(), WithEndpoint(api.URL))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := client.Service("iam", false).Call(context.Background(), Request{Method: http.MethodGet, Path: "/v1beta1/organizations/o"}, &structpb.Struct{}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(auth, ", ") != "Bearer sa-token-1, Bearer sa-token-1" {
		t.Fatalf("Authorization = %v, want the service account's one token twice", auth)
	}
	if n := sts.issued.Load(); n != 1 {
		t.Fatalf("%d exchanges, want 1", n)
	}
}

func TestATokenAboutToExpireIsRenewedWithoutAFailedCall(t *testing.T) {
	path, sts := newKeyFile(t)
	credentials, err := CredentialsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The API refuses any token but the newest, as it would an expired one.
	var calls, refused atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != fmt.Sprintf("Bearer sa-token-%d", sts.issued.Load()) {
			refused.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(api.Close)
	client, err := NewClient(context.Background(), WithEndpoint(api.URL), WithCredentials(credentials), WithMaxAttempts(1))
	if err != nil {
		t.Fatal(err)
	}
	call := func() {
		t.Helper()
		if err := client.Service("iam", false).Call(context.Background(), Request{Method: http.MethodGet, Path: "/v1beta1/organizations/o"}, &structpb.Struct{}); err != nil {
			t.Fatal(err)
		}
	}

	call()
	// The token now expires within the renewal margin: still valid, but
	// replaced before the next call is sent.
	credentials.cache.mu.Lock()
	credentials.cache.expiry = time.Now().Add(federatedExpiryMargin / 2)
	credentials.cache.mu.Unlock()
	call()

	if n := sts.issued.Load(); n != 2 {
		t.Fatalf("%d exchanges, want 2", n)
	}
	if calls.Load() != 2 || refused.Load() != 0 {
		t.Fatalf("%d calls, %d refused; want 2 and none", calls.Load(), refused.Load())
	}
	sts.mu.Lock()
	defer sts.mu.Unlock()
	if sts.claim[0]["jti"] == sts.claim[1]["jti"] {
		t.Fatal("two assertions shared a jti")
	}
}

func TestConcurrentCallersShareOneKeyExchange(t *testing.T) {
	path, sts := newKeyFile(t)
	credentials, err := CredentialsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if token, err := credentials.Token(context.Background()); err != nil || token != "sa-token-1" {
				t.Errorf("Token() = %q, %v; want sa-token-1", token, err)
			}
		})
	}
	wg.Wait()
	if n := sts.issued.Load(); n != 1 {
		t.Fatalf("%d exchanges, want 1", n)
	}
}

func TestTheAssertionLivesAtMostAnHour(t *testing.T) {
	path, sts := newKeyFile(t)
	credentials, err := CredentialsFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := credentials.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	claims := sts.claim[0]
	if lifetime := claims["exp"].(float64) - claims["iat"].(float64); lifetime <= 0 || lifetime > 3600 {
		t.Fatalf("the assertion lives %vs", lifetime)
	}
}

func TestARefusedAssertionNamesTheKey(t *testing.T) {
	sts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
	}))
	t.Cleanup(sts.Close)
	private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	credentials, err := CredentialsFromJSON(credentialsJSON(t, private, sts.URL))
	if err != nil {
		t.Fatal(err)
	}
	_, err = credentials.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), testKeyName) || !strings.Contains(err.Error(), "audit log") {
		t.Fatalf("err = %v, want the key named and the audit log pointed at", err)
	}
}

func TestAnUnavailableSTSIsAskedAgain(t *testing.T) {
	private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	real := newKeySTS(t, &private.PublicKey)
	var attempts atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		real.serve(w, r)
	}))
	t.Cleanup(flaky.Close)
	// The assertion's audience is the flaky front's, so the fake checks
	// against it.
	real.URL = flaky.URL
	credentials, err := CredentialsFromJSON(credentialsJSON(t, private, flaky.URL))
	if err != nil {
		t.Fatal(err)
	}
	if token, err := credentials.Token(context.Background()); err != nil || token != "sa-token-1" {
		t.Fatalf("Token() = %q, %v; want sa-token-1 after a retry", token, err)
	}
}

func TestACredentialsFileThatIsntOneIsRefused(t *testing.T) {
	private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	good := map[string]string{}
	_ = json.Unmarshal(credentialsJSON(t, private, "https://sts.origincloudapis.eu"), &good)
	rsaLike := strings.Replace(good["private_key"], "MI", "MX", 1)
	for name, change := range map[string]func(map[string]string){
		"another type":  func(f map[string]string) { f["type"] = "service_account" },
		"another alg":   func(f map[string]string) { f["algorithm"] = "RS256" },
		"no name":       func(f map[string]string) { delete(f, "name") },
		"no account":    func(f map[string]string) { delete(f, "service_account") },
		"no endpoint":   func(f map[string]string) { delete(f, "sts_endpoint") },
		"no key":        func(f map[string]string) { delete(f, "private_key") },
		"a damaged key": func(f map[string]string) { f["private_key"] = rsaLike },
		"a public key": func(f map[string]string) {
			f["private_key"] = strings.ReplaceAll(f["private_key"], "PRIVATE", "PUBLIC")
		},
		"a relative url": func(f map[string]string) { f["sts_endpoint"] = "sts.origincloudapis.eu" },
	} {
		t.Run(name, func(t *testing.T) {
			file := map[string]string{}
			for k, v := range good {
				file[k] = v
			}
			change(file)
			data, _ := json.Marshal(file)
			_, err := CredentialsFromJSON(data)
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE KEY") || strings.Contains(err.Error(), good["private_key"][30:60]) {
				t.Fatalf("the error quotes the key: %v", err)
			}
		})
	}
	if _, err := CredentialsFromJSON([]byte("not json " + good["private_key"])); err == nil || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatalf("err = %v, want a refusal that doesn't quote the file", err)
	}
}

func TestTheCredentialsNeverPrintThePrivateKey(t *testing.T) {
	private, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	credentials, err := CredentialsFromJSON(credentialsJSON(t, private, "https://sts.origincloudapis.eu"))
	if err != nil {
		t.Fatal(err)
	}
	secret := private.D.String()
	secretHex := private.D.Text(16)
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%x", "%d"} {
		out := fmt.Sprintf(verb, credentials)
		if strings.Contains(out, secret) || strings.Contains(out, secretHex) {
			t.Fatalf("%s prints the private key: %s", verb, out)
		}
	}
	if credentials.Key() != testKeyName || credentials.ServiceAccount() != testServiceAccount {
		t.Fatalf("Key() = %q, ServiceAccount() = %q", credentials.Key(), credentials.ServiceAccount())
	}
}

// The chain: an access token, then a credentials file, then a CI job's
// OIDC token, then the CLI's session, then the instance's metadata service.
func TestTheCredentialChainOrder(t *testing.T) {
	keyFile, _ := newKeyFile(t)
	jobToken := filepath.Join(t.TempDir(), "job-token")
	if err := os.WriteFile(jobToken, []byte("job"), 0o600); err != nil {
		t.Fatal(err)
	}
	type source struct {
		name string
		set  func(t *testing.T, dataDir string)
	}
	sources := []source{
		{"access token", func(t *testing.T, _ string) { t.Setenv(envAccessToken, "from-env") }},
		{"credentials file", func(t *testing.T, _ string) { t.Setenv(envCredentialsFile, keyFile) }},
		{"CI job's OIDC token", func(t *testing.T, _ string) { t.Setenv(envOIDCTokenFile, jobToken) }},
		{"CLI session", func(t *testing.T, dir string) {
			writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
		}},
	}
	kind := func(c Credentials) string {
		switch c.(type) {
		case staticToken:
			return "access token"
		case *ServiceAccountKeyCredentials:
			return "credentials file"
		case *FederatedCredentials:
			return "CI job's OIDC token"
		case *CLISessionCredentials:
			return "CLI session"
		case *InstanceMetadataCredentials:
			return "metadata service"
		}
		return fmt.Sprintf("%T", c)
	}
	// With every source from the i-th on present, the i-th is taken.
	for i, want := range sources {
		t.Run(want.name, func(t *testing.T) {
			dir := clearCredentialEnv(t)
			for _, s := range sources[i:] {
				s.set(t, dir)
			}
			credentials, err := DefaultCredentials(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got := kind(credentials); got != want.name {
				t.Fatalf("took the %s, want the %s", got, want.name)
			}
		})
	}
	t.Run("metadata service", func(t *testing.T) {
		clearCredentialEnv(t)
		credentials, err := DefaultCredentials(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := kind(credentials); got != "metadata service" {
			t.Fatalf("took the %s, want the metadata service", got)
		}
	})
}

// A credentials file the environment names but that can't be read is an
// error, not a step passed over: the program meant to act as that account.
func TestAnUnreadableCredentialsFileIsAnError(t *testing.T) {
	dir := clearCredentialEnv(t)
	writeSession(t, dir, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	t.Setenv(envCredentialsFile, filepath.Join(dir, "missing.json"))

	if _, err := DefaultCredentials(context.Background()); err == nil || !strings.Contains(err.Error(), "credentials file") {
		t.Fatalf("err = %v, want the credentials file's", err)
	}
}
