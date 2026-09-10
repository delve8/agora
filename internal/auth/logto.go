package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type LogtoValidatorOptions struct {
	Issuer     string
	Audience   string
	HTTPClient *http.Client
	Clock      func() time.Time
}

type LogtoTokenValidator struct {
	options       LogtoValidatorOptions
	client        *http.Client
	clock         func() time.Time
	mu            sync.RWMutex
	discovery     *oidcDiscovery
	keys          map[string]cryptoKey
	keysFetchedAt time.Time
}

type oidcDiscovery struct {
	Issuer  string `json:"issuer"`
	JWKSURI string `json:"jwks_uri"`
}
type jwksDocument struct {
	Keys []jsonWebKey `json:"keys"`
}
type jsonWebKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}
type cryptoKey struct {
	algorithm string
	public    any
}

func NewLogtoTokenValidator(options LogtoValidatorOptions) (*LogtoTokenValidator, error) {
	if strings.TrimSpace(options.Issuer) == "" {
		return nil, errors.New("logto issuer is required")
	}
	if strings.TrimSpace(options.Audience) == "" {
		return nil, errors.New("logto audience is required")
	}
	if options.HTTPClient == nil {
		options.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	return &LogtoTokenValidator{options: options, client: options.HTTPClient, clock: options.Clock}, nil
}

func (v *LogtoTokenValidator) Validate(ctx context.Context, raw string) (Token, error) {
	parts := strings.Split(strings.TrimSpace(raw), ".")
	if len(parts) != 3 {
		return Token{}, errors.New("invalid JWT format")
	}
	var header map[string]any
	var claims map[string]any
	if err := decodeSegment(parts[0], &header); err != nil {
		return Token{}, fmt.Errorf("decode JWT header: %w", err)
	}
	if err := decodeSegment(parts[1], &claims); err != nil {
		return Token{}, fmt.Errorf("decode JWT payload: %w", err)
	}
	if err := v.validateClaims(claims); err != nil {
		return Token{}, err
	}
	alg, _ := header["alg"].(string)
	kid, _ := header["kid"].(string)
	key, err := v.signingKey(ctx, kid, alg)
	if err != nil {
		return Token{}, err
	}
	if err := verifySignature(parts, alg, key); err != nil {
		return Token{}, err
	}
	subject, _ := claims["sub"].(string)
	if strings.TrimSpace(subject) == "" {
		return Token{}, errors.New("token is missing sub claim")
	}
	provider := ProviderLogto
	// The display label prefers the login handle: Logto carries it in "username"
	// and OIDC in "preferred_username", while "name" is an optional profile
	// display name that is frequently empty (Logto's bootstrap users have no
	// name). The stable identity remains (provider, subject).
	return Token{Subject: subject, Provider: provider, DisplayName: stringClaim(claims, "username", "preferred_username", "name"), Email: stringClaim(claims, "email"), EmailVerified: boolClaim(claims, "email_verified")}, nil
}

func (v *LogtoTokenValidator) validateClaims(claims map[string]any) error {
	issuer, _ := claims["iss"].(string)
	if strings.TrimRight(issuer, "/") != strings.TrimRight(v.options.Issuer, "/") {
		return errors.New("token issuer does not match configured issuer")
	}
	if !audienceContains(claims["aud"], v.options.Audience) {
		return errors.New("token audience does not match configured audience")
	}
	exp, ok := numericDate(claims["exp"])
	if !ok {
		return errors.New("token is missing or has invalid exp claim")
	}
	if time.Unix(exp, 0).Before(v.clock().Add(-30 * time.Second)) {
		return errors.New("token is expired")
	}
	return nil
}

func audienceContains(value any, want string) bool {
	switch v := value.(type) {
	case string:
		return v == want
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && s == want {
				return true
			}
		}
	}
	return false
}
func numericDate(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), v > 0
	case json.Number:
		n, err := v.Int64()
		return n, err == nil && n > 0
	case int64:
		return v, v > 0
	}
	return 0, false
}
func stringClaim(claims map[string]any, names ...string) string {
	for _, name := range names {
		if value, ok := claims[name].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func boolClaim(claims map[string]any, name string) bool {
	value, _ := claims[name].(bool)
	return value
}

func (v *LogtoTokenValidator) signingKey(ctx context.Context, kid, alg string) (cryptoKey, error) {
	if err := v.ensureKeys(ctx); err != nil {
		return cryptoKey{}, err
	}
	v.mu.RLock()
	key, ok := v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return cryptoKey{}, fmt.Errorf("no signing key for kid %q", kid)
	}
	if alg == "" || key.algorithm != alg {
		return cryptoKey{}, errors.New("unsupported or mismatched JWT algorithm")
	}
	return key, nil
}
func (v *LogtoTokenValidator) ensureKeys(ctx context.Context) error {
	v.mu.RLock()
	recent := v.discovery != nil && len(v.keys) > 0 && time.Since(v.keysFetchedAt) < 5*time.Minute
	v.mu.RUnlock()
	if recent {
		return nil
	}
	if err := v.fetchDiscovery(ctx); err != nil {
		return err
	}
	return v.fetchKeys(ctx)
}
func (v *LogtoTokenValidator) fetchDiscovery(ctx context.Context) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(v.options.Issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch OIDC discovery: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("OIDC discovery returned %d", response.StatusCode)
	}
	var document oidcDiscovery
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return err
	}
	if strings.TrimRight(document.Issuer, "/") != strings.TrimRight(v.options.Issuer, "/") || document.JWKSURI == "" {
		return errors.New("OIDC discovery document is invalid")
	}
	parsed, err := url.Parse(document.JWKSURI)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return errors.New("JWKS URI must use https")
	}
	v.mu.Lock()
	v.discovery = &document
	v.mu.Unlock()
	return nil
}
func (v *LogtoTokenValidator) fetchKeys(ctx context.Context) error {
	v.mu.RLock()
	document := v.discovery
	v.mu.RUnlock()
	if document == nil {
		return errors.New("OIDC discovery not loaded")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, document.JWKSURI, nil)
	if err != nil {
		return err
	}
	response, err := v.client.Do(request)
	if err != nil {
		return fmt.Errorf("fetch JWKS: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("JWKS returned %d", response.StatusCode)
	}
	var jwks jwksDocument
	if err := json.NewDecoder(response.Body).Decode(&jwks); err != nil {
		return err
	}
	keys := make(map[string]cryptoKey)
	for _, item := range jwks.Keys {
		key, err := parseJWK(item)
		if err == nil && item.Kid != "" {
			keys[item.Kid] = key
		}
	}
	if len(keys) == 0 {
		return errors.New("no usable signing keys in JWKS")
	}
	v.mu.Lock()
	v.keys = keys
	v.keysFetchedAt = v.clock()
	v.mu.Unlock()
	return nil
}
func parseJWK(item jsonWebKey) (cryptoKey, error) {
	switch item.Kty {
	case "RSA":
		n, err := base64.RawURLEncoding.DecodeString(item.N)
		if err != nil {
			return cryptoKey{}, err
		}
		e, err := base64.RawURLEncoding.DecodeString(item.E)
		if err != nil {
			return cryptoKey{}, err
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 | int(b)
		}
		return cryptoKey{algorithm: "RS256", public: &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}}, nil
	case "EC":
		x, err := base64.RawURLEncoding.DecodeString(item.X)
		if err != nil {
			return cryptoKey{}, err
		}
		y, err := base64.RawURLEncoding.DecodeString(item.Y)
		if err != nil {
			return cryptoKey{}, err
		}
		curve, algorithm, err := curveFor(item.Crv)
		if err != nil {
			return cryptoKey{}, err
		}
		return cryptoKey{algorithm: algorithm, public: &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}}, nil
	default:
		return cryptoKey{}, fmt.Errorf("unsupported key type %q", item.Kty)
	}
}
func curveFor(name string) (elliptic.Curve, string, error) {
	switch name {
	case "P-256":
		return elliptic.P256(), "ES256", nil
	case "P-384":
		return elliptic.P384(), "ES384", nil
	default:
		return nil, "", errors.New("unsupported EC curve")
	}
}
func verifySignature(parts []string, alg string, key cryptoKey) error {
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	content := []byte(parts[0] + "." + parts[1])
	switch public := key.public.(type) {
	case *rsa.PublicKey:
		digest := sha256.Sum256(content)
		if alg != "RS256" || rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature) != nil {
			return errors.New("invalid JWT signature")
		}
	case *ecdsa.PublicKey:
		if alg == "ES256" {
			digest := sha256.Sum256(content)
			return verifyECDSA(public, digest[:], signature)
		}
		if alg == "ES384" {
			digest := sha512.Sum384(content)
			return verifyECDSA(public, digest[:], signature)
		}
		return errors.New("unsupported JWT algorithm")
	default:
		return errors.New("unsupported signing key")
	}
	return nil
}
func verifyECDSA(public *ecdsa.PublicKey, digest, signature []byte) error {
	if len(signature)%2 != 0 {
		return errors.New("invalid ECDSA signature length")
	}
	half := len(signature) / 2
	if !ecdsa.Verify(public, digest, new(big.Int).SetBytes(signature[:half]), new(big.Int).SetBytes(signature[half:])) {
		return errors.New("invalid JWT signature")
	}
	return nil
}
func decodeSegment(segment string, target any) error {
	decoded, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(decoded, target)
}
func isLoopbackHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "localhost" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
