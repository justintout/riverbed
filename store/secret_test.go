package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	hex, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseKey(hex)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestParseKey(t *testing.T) {
	hex, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(hex) != KeyBytes*2 {
		t.Errorf("a new key is %d characters, want %d", len(hex), KeyBytes*2)
	}
	key, err := ParseKey(hex)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeyBytes {
		t.Errorf("key is %d bytes, want %d", len(key), KeyBytes)
	}

	// Surrounding whitespace is common when a key comes from a file.
	if _, err := ParseKey("  " + hex + "\n"); err != nil {
		t.Errorf("a padded key should parse: %v", err)
	}

	for name, bad := range map[string]string{
		"empty":      "",
		"not hex":    strings.Repeat("z", 64),
		"too short":  strings.Repeat("ab", 8),
		"too long":   strings.Repeat("ab", 64),
		"odd length": strings.Repeat("a", 63),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKey(bad); err == nil {
				t.Error("want an error")
			}
		})
	}
}

func TestNewKeyIsRandom(t *testing.T) {
	first, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two keys must differ")
	}
}

func TestSealRoundTrip(t *testing.T) {
	key := testKey(t)

	sealed, err := seal(key, label("notion", "access_token"), "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sealed, "secret-token") {
		t.Fatalf("the plaintext survived: %q", sealed)
	}
	if !strings.HasPrefix(sealed, sealPrefix) {
		t.Errorf("sealed value = %q, want the scheme prefix", sealed)
	}

	got, err := unseal(key, label("notion", "access_token"), sealed)
	if err != nil {
		t.Fatal(err)
	}
	if got != "secret-token" {
		t.Errorf("opened %q", got)
	}
}

func TestSealIsNotDeterministic(t *testing.T) {
	key := testKey(t)
	first, err := seal(key, "l", "same-value")
	if err != nil {
		t.Fatal(err)
	}
	second, err := seal(key, "l", "same-value")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("a fresh nonce should make each ciphertext different")
	}
}

func TestOpenRejectsTheWrongKey(t *testing.T) {
	sealed, err := seal(testKey(t), "l", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unseal(testKey(t), "l", sealed); err == nil {
		t.Error("another key must not open it")
	}
}

func TestOpenRejectsADifferentField(t *testing.T) {
	// A ciphertext must not be movable between columns or servers.
	key := testKey(t)
	sealed, err := seal(key, label("notion", "refresh_token"), "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unseal(key, label("notion", "access_token"), sealed); err == nil {
		t.Error("the field is authenticated, so this must fail")
	}
	if _, err := unseal(key, label("other", "refresh_token"), sealed); err == nil {
		t.Error("the server is authenticated, so this must fail")
	}
}

func TestOpenRejectsTampering(t *testing.T) {
	key := testKey(t)
	sealed, err := seal(key, "l", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	tampered := sealed[:len(sealed)-1] + string(sealed[len(sealed)-1]^1)
	if _, err := unseal(key, "l", tampered); err == nil {
		t.Error("a modified ciphertext must fail")
	}
}

func TestOpenReportsAMissingKey(t *testing.T) {
	sealed, err := seal(testKey(t), "l", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = unseal(nil, "l", sealed)
	if err == nil || !strings.Contains(err.Error(), "no secret key") {
		t.Errorf("want a clear error about the missing key, got %v", err)
	}
}

func TestOpenReportsPlaintextWhenAKeyIsSet(t *testing.T) {
	_, err := unseal(testKey(t), "l", "plain-token")
	if err == nil || !strings.Contains(err.Error(), "not encrypted") {
		t.Errorf("want a clear error telling the user to re-authorize, got %v", err)
	}
}

func TestWithoutAKeyValuesPassThrough(t *testing.T) {
	sealed, err := seal(nil, "l", "plain-token")
	if err != nil {
		t.Fatal(err)
	}
	if sealed != "plain-token" {
		t.Errorf("sealed = %q", sealed)
	}
	got, err := unseal(nil, "l", sealed)
	if err != nil || got != "plain-token" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestEmptyValuesStayEmpty(t *testing.T) {
	key := testKey(t)
	sealed, err := seal(key, "l", "")
	if err != nil || sealed != "" {
		t.Errorf("sealed = %q, %v", sealed, err)
	}
	got, err := unseal(key, "l", "")
	if err != nil || got != "" {
		t.Errorf("opened %q, %v", got, err)
	}
}

// TestCredentialsAreNotReadableOnDisk is the point of the whole feature: a copy
// of the database must not hand over a working token.
func TestCredentialsAreNotReadableOnDisk(t *testing.T) {
	const (
		accessToken  = "ACCESS-TOKEN-fedcba9876543210"
		refreshToken = "REFRESH-TOKEN-0123456789abcdef"
		clientSecret = "CLIENT-SECRET-abcdefabcdefabcd"
	)

	dir := t.TempDir()
	path := filepath.Join(dir, "riverbed.db")
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseKey(key)
	if err != nil {
		t.Fatal(err)
	}

	s, err := Open(t.Context(), Options{Path: path, PoolSize: 2, SecretKey: parsed})
	if err != nil {
		t.Fatal(err)
	}
	if !s.CredentialsEncrypted() {
		t.Fatal("credentials should be encrypted")
	}
	if err := s.PutToken(t.Context(), "notion", &oauth2.Token{
		AccessToken: accessToken, RefreshToken: refreshToken,
		TokenType: "Bearer", Expiry: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutClient(t.Context(), "notion", &OAuthClient{
		ClientID: "visible-client-id", ClientSecret: clientSecret,
		TokenURL: "https://as.example.invalid/token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Everything the database wrote, including the journal files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []byte
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		onDisk = append(onDisk, body...)
	}

	for name, secret := range map[string]string{
		"access token":  accessToken,
		"refresh token": refreshToken,
		"client secret": clientSecret,
	} {
		if bytes.Contains(onDisk, []byte(secret)) {
			t.Errorf("the %s is readable in the database files", name)
		}
	}
	// A control, so the search itself is known to work.
	if !bytes.Contains(onDisk, []byte("visible-client-id")) {
		t.Error("the client id should be present, or this test proves nothing")
	}

	// Reading it back through the store still works.
	reopened, err := Open(t.Context(), Options{Path: path, PoolSize: 2, SecretKey: parsed})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	tok, err := reopened.Token(t.Context(), "notion")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != accessToken || tok.RefreshToken != refreshToken {
		t.Errorf("token = %+v", tok)
	}
	client, err := reopened.Client(t.Context(), "notion")
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientSecret != clientSecret {
		t.Errorf("client secret = %q", client.ClientSecret)
	}
}

func TestADifferentKeyCannotReadStoredCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "riverbed.db")

	first := testKey(t)
	s, err := Open(t.Context(), Options{Path: path, PoolSize: 2, SecretKey: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutToken(t.Context(), "notion", &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(t.Context(), Options{Path: path, PoolSize: 2, SecretKey: testKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	_, err = second.Token(t.Context(), "notion")
	if err == nil {
		t.Fatal("a different key must not read the token")
	}
	if !strings.Contains(err.Error(), "secret key may have changed") {
		t.Errorf("the error should say what is wrong: %v", err)
	}
}

func TestOpenRejectsAKeyOfTheWrongLength(t *testing.T) {
	_, err := Open(t.Context(), Options{
		Path: filepath.Join(t.TempDir(), "riverbed.db"), PoolSize: 2,
		SecretKey: []byte("too-short"),
	})
	if err == nil || !strings.Contains(err.Error(), "secret key") {
		t.Errorf("want a key length error, got %v", err)
	}
}
