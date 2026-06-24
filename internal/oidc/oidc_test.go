package oidc

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signToken builds a minimal RS256 JWT for tests.
func signToken(t *testing.T, priv *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": kid}
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	signingInput := enc(header) + "." + enc(claims)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// jwksServer serves a JWKS containing one RSA public key.
func jwksServer(t *testing.T, pub *rsa.PublicKey, kid string) *httptest.Server {
	t.Helper()
	eBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(eBuf, uint64(pub.E))
	eBytes := strings.TrimLeft(string(eBuf), "\x00")
	jwks := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString([]byte(eBytes)),
	}}}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jwks)
	}))
}

func TestVerifyValidToken(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	srv := jwksServer(t, &priv.PublicKey, "kid-1")
	defer srv.Close()

	v := New(Config{JWKSURL: srv.URL, Issuer: "https://issuer", Audience: "frostgate"})
	v.now = func() time.Time { return time.Unix(1_000_000, 0) }

	token := signToken(t, priv, "kid-1", map[string]any{
		"sub": "user-42", "iss": "https://issuer", "aud": "frostgate",
		"exp": 1_000_100,
	})
	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Sub != "user-42" {
		t.Fatalf("sub mismatch: %q", claims.Sub)
	}
}

func TestRejectExpired(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &priv.PublicKey, "kid-1")
	defer srv.Close()
	v := New(Config{JWKSURL: srv.URL})
	v.now = func() time.Time { return time.Unix(1_000_000, 0) }
	token := signToken(t, priv, "kid-1", map[string]any{"sub": "u", "exp": 1})
	if _, err := v.Verify(token); err == nil {
		t.Fatalf("expected expired token rejection")
	}
}

func TestRejectBadSignature(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &priv.PublicKey, "kid-1")
	defer srv.Close()
	v := New(Config{JWKSURL: srv.URL})
	v.now = func() time.Time { return time.Unix(1_000_000, 0) }
	// Sign with the wrong key.
	token := signToken(t, other, "kid-1", map[string]any{"sub": "u", "exp": 1_000_100})
	if _, err := v.Verify(token); err == nil {
		t.Fatalf("expected signature rejection")
	}
}

func TestRejectWrongIssuer(t *testing.T) {
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	srv := jwksServer(t, &priv.PublicKey, "kid-1")
	defer srv.Close()
	v := New(Config{JWKSURL: srv.URL, Issuer: "https://expected"})
	v.now = func() time.Time { return time.Unix(1_000_000, 0) }
	token := signToken(t, priv, "kid-1", map[string]any{
		"sub": "u", "iss": "https://attacker", "exp": 1_000_100,
	})
	if _, err := v.Verify(token); err == nil {
		t.Fatalf("expected issuer rejection")
	}
}
