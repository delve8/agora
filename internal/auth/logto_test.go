package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type jwtKeyFixture struct {
	kid     string
	alg     string
	jwk     jsonWebKey
	private any
}

func newRSKey(t *testing.T, kid string) jwtKeyFixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return jwtKeyFixture{
		kid:     kid,
		alg:     "RS256",
		private: key,
		jwk: jsonWebKey{
			Kty: "RSA", Kid: kid, Alg: "RS256",
			N: base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString([]byte{0x01, 0x00, 0x01}),
		},
	}
}

func newECKey(t *testing.T, curve elliptic.Curve, alg, kid string) jwtKeyFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	size := (curve.Params().BitSize + 7) / 8
	crv := ""
	switch alg {
	case "ES256":
		crv = "P-256"
	case "ES384":
		crv = "P-384"
	}
	x := make([]byte, size)
	y := make([]byte, size)
	key.PublicKey.X.FillBytes(x)
	key.PublicKey.Y.FillBytes(y)
	return jwtKeyFixture{
		kid:     kid,
		alg:     alg,
		private: key,
		jwk: jsonWebKey{
			Kty: "EC", Kid: kid, Alg: alg, Crv: crv,
			X: base64.RawURLEncoding.EncodeToString(x),
			Y: base64.RawURLEncoding.EncodeToString(y),
		},
	}
}

func ecSignature(r, s *big.Int, size int) []byte {
	out := make([]byte, 2*size)
	r.FillBytes(out[:size])
	s.FillBytes(out[size:])
	return out
}

func signJWT(t *testing.T, fixture jwtKeyFixture, header, claims map[string]any) string {
	t.Helper()
	headerBytes, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsBytes, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerBytes) + "." + base64.RawURLEncoding.EncodeToString(claimsBytes)
	var signature []byte
	switch private := fixture.private.(type) {
	case *rsa.PrivateKey:
		digest := sha256.Sum256([]byte(signingInput))
		signature, err = rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, digest[:])
	case *ecdsa.PrivateKey:
		var r, s *big.Int
		if fixture.alg == "ES256" {
			digest := sha256.Sum256([]byte(signingInput))
			r, s, err = ecdsa.Sign(rand.Reader, private, digest[:])
			signature = ecSignature(r, s, 32)
		} else {
			digest := sha512.Sum384([]byte(signingInput))
			r, s, err = ecdsa.Sign(rand.Reader, private, digest[:])
			signature = ecSignature(r, s, 48)
		}
	default:
		t.Fatalf("unsupported signing key type %T", fixture.private)
	}
	if err != nil {
		t.Fatalf("sign JWT: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// newLogtoServer serves OIDC discovery + JWKS for the given keys on a loopback
// server and reports how many discovery/JWKS requests the validator made.
// The discovery document's issuer is server.URL unless discoveryIssuer is set.
func newLogtoServer(t *testing.T, keys []jwtKeyFixture, discoveryIssuer string) (*httptest.Server, *int) {
	t.Helper()
	fetches := 0
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		issuer := discoveryIssuer
		if issuer == "" {
			issuer = server.URL
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "jwks_uri": server.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Header().Set("Content-Type", "application/json")
		document := jwksDocument{Keys: make([]jsonWebKey, 0, len(keys))}
		for _, key := range keys {
			document.Keys = append(document.Keys, key.jwk)
		}
		_ = json.NewEncoder(w).Encode(document)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &fetches
}

func tokenClaims(issuer, audience string) map[string]any {
	return map[string]any{
		"iss": issuer, "aud": audience, "sub": "subject-1",
		"exp":  time.Now().Add(time.Hour).Unix(),
		"name": "Test User", "email": "user@example.com", "email_verified": true,
	}
}

func newValidator(t *testing.T, issuer, audience string) *LogtoTokenValidator {
	t.Helper()
	validator, err := NewLogtoTokenValidator(LogtoValidatorOptions{Issuer: issuer, Audience: audience})
	if err != nil {
		t.Fatalf("NewLogtoTokenValidator: %v", err)
	}
	return validator
}

func TestLogtoValidatorRS256(t *testing.T) {
	key := newRSKey(t, "rsa-key")
	server, fetches := newLogtoServer(t, []jwtKeyFixture{key}, "")
	validator := newValidator(t, server.URL, "agora-api")
	token := signJWT(t, key, map[string]any{"alg": "RS256", "kid": key.kid}, tokenClaims(server.URL, "agora-api"))

	got, err := validator.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Subject != "subject-1" || got.Provider != ProviderLogto || got.DisplayName != "Test User" || got.Email != "user@example.com" || !got.EmailVerified {
		t.Fatalf("unexpected token: %+v", got)
	}
	// Second validation must hit the JWKS/discovery cache.
	if _, err := validator.Validate(context.Background(), token); err != nil {
		t.Fatalf("cached Validate: %v", err)
	}
	if *fetches != 2 {
		t.Fatalf("expected discovery + JWKS fetched exactly once, got %d requests", *fetches)
	}
}

func TestLogtoValidatorAudienceList(t *testing.T) {
	key := newRSKey(t, "rsa-key")
	server, _ := newLogtoServer(t, []jwtKeyFixture{key}, "")
	validator := newValidator(t, server.URL, "agora-api")
	claims := tokenClaims(server.URL, "agora-api")
	claims["aud"] = []any{"other-resource", "agora-api"}
	token := signJWT(t, key, map[string]any{"alg": "RS256", "kid": key.kid}, claims)
	if _, err := validator.Validate(context.Background(), token); err != nil {
		t.Fatalf("Validate with aud list: %v", err)
	}
}

func TestLogtoValidatorES256AndES384(t *testing.T) {
	for _, fixture := range []jwtKeyFixture{
		newECKey(t, elliptic.P256(), "ES256", "ec-p256"),
		newECKey(t, elliptic.P384(), "ES384", "ec-p384"),
	} {
		server, _ := newLogtoServer(t, []jwtKeyFixture{fixture}, "")
		validator := newValidator(t, server.URL, "agora-api")
		token := signJWT(t, fixture, map[string]any{"alg": fixture.alg, "kid": fixture.kid}, tokenClaims(server.URL, "agora-api"))
		if _, err := validator.Validate(context.Background(), token); err != nil {
			t.Errorf("%s token rejected: %v", fixture.alg, err)
		}
	}
}

func TestLogtoValidatorRejects(t *testing.T) {
	key := newRSKey(t, "rsa-key")
	other := newRSKey(t, "other-key")
	server, _ := newLogtoServer(t, []jwtKeyFixture{key}, "")
	validator := newValidator(t, server.URL, "agora-api")

	mutate := func(change func(map[string]any)) map[string]any {
		claims := tokenClaims(server.URL, "agora-api")
		change(claims)
		return claims
	}
	for _, value := range []struct {
		name   string
		header map[string]any
		claims map[string]any
		signer jwtKeyFixture
	}{
		{name: "wrong issuer", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) { c["iss"] = "https://evil.example.com" }), signer: key},
		{name: "wrong audience", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) { c["aud"] = "other-api" }), signer: key},
		{name: "expired", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), signer: key},
		{name: "missing exp", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) { delete(c, "exp") }), signer: key},
		{name: "missing sub", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) { delete(c, "sub") }), signer: key},
		{name: "wrong signature", header: map[string]any{"alg": "RS256", "kid": key.kid}, claims: mutate(func(c map[string]any) {}), signer: other},
		{name: "unknown kid", header: map[string]any{"alg": "RS256", "kid": "missing-key"}, claims: mutate(func(c map[string]any) {}), signer: key},
		{name: "unsupported alg", header: map[string]any{"alg": "HS256", "kid": key.kid}, claims: mutate(func(c map[string]any) {}), signer: key},
	} {
		t.Run(value.name, func(t *testing.T) {
			token := signJWT(t, value.signer, value.header, value.claims)
			if _, err := validator.Validate(context.Background(), token); err == nil {
				t.Fatalf("expected validation failure for %q", value.name)
			}
		})
	}
}

func TestLogtoValidatorDiscoveryIssuerMismatch(t *testing.T) {
	key := newRSKey(t, "rsa-key")
	server, _ := newLogtoServer(t, []jwtKeyFixture{key}, "https://different.example.com")
	validator := newValidator(t, server.URL, "agora-api")
	token := signJWT(t, key, map[string]any{"alg": "RS256", "kid": key.kid}, tokenClaims(server.URL, "agora-api"))
	if _, err := validator.Validate(context.Background(), token); err == nil {
		t.Fatal("expected validation failure when discovery issuer differs from configured issuer")
	}
}

func TestLogtoValidatorRejectsNonLoopbackJWKS(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": server.URL, "jwks_uri": "http://example.com/jwks"})
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	validator := newValidator(t, server.URL, "agora-api")
	token := signJWT(t, newRSKey(t, "rsa-key"), map[string]any{"alg": "RS256", "kid": "rsa-key"}, tokenClaims(server.URL, "agora-api"))
	if _, err := validator.Validate(context.Background(), token); err == nil {
		t.Fatal("expected validation failure for non-loopback http JWKS URI")
	}
}
