// Package oidc validates RS256 JWT bearer tokens against a JWKS endpoint using
// only the standard library. It is intentionally minimal: enough to
// authenticate machine/user tokens from an OIDC provider (Auth0, Okta, Entra,
// Keycloak, ...) without pulling in a JWT dependency.
package oidc

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Verifier validates tokens and caches the provider's signing keys.
type Verifier struct {
	jwksURL  string
	issuer   string
	audience string
	client   *http.Client
	now      func() time.Time // injectable clock for tests

	mu      sync.RWMutex
	keys    map[string]*rsa.PublicKey // kid -> key
	fetched time.Time
}

// Config configures a Verifier.
type Config struct {
	JWKSURL  string
	Issuer   string
	Audience string
}

// New builds a Verifier.
func New(cfg Config) *Verifier {
	return &Verifier{
		jwksURL:  cfg.JWKSURL,
		issuer:   cfg.Issuer,
		audience: cfg.Audience,
		client:   &http.Client{Timeout: 10 * time.Second},
		now:      time.Now,
		keys:     map[string]*rsa.PublicKey{},
	}
}

// Claims is the validated subset of token claims we use.
type Claims struct {
	Sub string
	Iss string
	Aud []string
	Exp int64
}

// rawClaims handles the aud field being either a string or an array.
type rawClaims struct {
	Sub string          `json:"sub"`
	Iss string          `json:"iss"`
	Aud json.RawMessage `json:"aud"`
	Exp int64           `json:"exp"`
}

// Verify validates a compact JWT and returns its claims, or an error.
func (v *Verifier) Verify(token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a compact JWT")
	}
	headerJSON, err := b64(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode header: %w", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, fmt.Errorf("parse header: %w", err)
	}
	if hdr.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported alg %q (only RS256)", hdr.Alg)
	}

	key, err := v.key(hdr.Kid)
	if err != nil {
		return nil, err
	}

	// Verify signature over "header.payload".
	signingInput := parts[0] + "." + parts[1]
	sig, err := b64(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig); err != nil {
		return nil, fmt.Errorf("signature invalid: %w", err)
	}

	// Decode and validate claims.
	payload, err := b64(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	var rc rawClaims
	if err := json.Unmarshal(payload, &rc); err != nil {
		return nil, fmt.Errorf("parse claims: %w", err)
	}
	now := v.now().Unix()
	if rc.Exp != 0 && now > rc.Exp+30 { // 30s leeway
		return nil, errors.New("token expired")
	}
	if v.issuer != "" && rc.Iss != v.issuer {
		return nil, fmt.Errorf("issuer mismatch: %q", rc.Iss)
	}
	aud := parseAud(rc.Aud)
	if v.audience != "" && !contains(aud, v.audience) {
		return nil, errors.New("audience mismatch")
	}
	if rc.Sub == "" {
		return nil, errors.New("missing sub claim")
	}
	return &Claims{Sub: rc.Sub, Iss: rc.Iss, Aud: aud, Exp: rc.Exp}, nil
}

// key returns the signing key for kid, refreshing the JWKS if needed.
func (v *Verifier) key(kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k := v.keys[kid]
	stale := v.now().Sub(v.fetched) > 10*time.Minute
	v.mu.RUnlock()
	if k != nil && !stale {
		return k, nil
	}
	if err := v.refresh(); err != nil && k == nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k := v.keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("no signing key for kid %q", kid)
}

// refresh fetches and parses the JWKS.
func (v *Verifier) refresh() error {
	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	var jwks struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := rsaKey(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = v.now()
	v.mu.Unlock()
	return nil
}

// rsaKey builds an *rsa.PublicKey from base64url modulus/exponent.
func rsaKey(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := b64(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := b64(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nBytes)
	// Exponent is a big-endian integer, usually 0x010001.
	var e uint64
	padded := make([]byte, 8)
	copy(padded[8-len(eBytes):], eBytes)
	e = binary.BigEndian.Uint64(padded)
	if e == 0 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{N: n, E: int(e)}, nil
}

func b64(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func parseAud(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
