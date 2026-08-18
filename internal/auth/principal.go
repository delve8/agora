package auth

import (
	"context"
	"fmt"
	"strings"
)

const (
	ModeLocal     = "local"
	ModeLogto     = "logto"
	ProviderLocal = "local"
	ProviderLogto = "logto"
)

type Principal struct {
	UserID      string `json:"user_id"`
	AuthSubject string `json:"auth_subject,omitempty"`
	Provider    string `json:"provider"`
	DisplayName string `json:"display_name,omitempty"`
	Email       string `json:"email,omitempty"`
}

type Token struct {
	Subject       string
	Provider      string
	DisplayName   string
	Email         string
	EmailVerified bool
}

func (t Token) Validate() error {
	if strings.TrimSpace(t.Subject) == "" {
		return fmt.Errorf("token subject must not be empty")
	}
	return nil
}

type ProvisionClaims struct {
	Provider      string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
}

type TokenValidator interface {
	Validate(context.Context, string) (Token, error)
}

type UserLookup interface {
	FindByAuthSubject(context.Context, string) (Principal, error)
}

type ClaimsUserLookup interface {
	UserLookup
	FindByClaims(context.Context, ProvisionClaims) (Principal, error)
}

type principalKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

func RequirePrincipal(ctx context.Context) (Principal, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok || strings.TrimSpace(p.UserID) == "" {
		return Principal{}, fmt.Errorf("authentication required")
	}
	return p, nil
}

func ExtractBearer(header string) string {
	parts := strings.Fields(header)
	if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
		return parts[1]
	}
	return ""
}

type Authenticator struct {
	mode         string
	validator    TokenValidator
	lookup       UserLookup
	local        Principal
	provisioning bool
}

func NewAuthenticator(mode string, validator TokenValidator, lookup UserLookup, local Principal) *Authenticator {
	return NewAuthenticatorWithProvisioning(mode, validator, lookup, local, true)
}

func NewAuthenticatorWithProvisioning(mode string, validator TokenValidator, lookup UserLookup, local Principal, provisioning bool) *Authenticator {
	if mode == "" {
		mode = ModeLocal
	}
	if local.UserID == "" {
		local = Principal{UserID: "local", Provider: ProviderLocal, DisplayName: "Local"}
	}
	return &Authenticator{mode: mode, validator: validator, lookup: lookup, local: local, provisioning: provisioning}
}

func (a *Authenticator) Mode() string { return a.mode }

func (a *Authenticator) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if a.mode == ModeLocal {
		return a.local, nil
	}
	if a.validator == nil || a.lookup == nil {
		return Principal{}, fmt.Errorf("authentication is not configured")
	}
	token, err := a.validator.Validate(ctx, bearer)
	if err != nil {
		return Principal{}, fmt.Errorf("invalid token: %w", err)
	}
	if err := token.Validate(); err != nil {
		return Principal{}, err
	}
	provider := token.Provider
	if provider == "" {
		provider = ProviderLogto
	}
	claims := ProvisionClaims{Provider: provider, Subject: token.Subject, Email: token.Email, EmailVerified: token.EmailVerified, DisplayName: token.DisplayName}
	if lookup, ok := a.lookup.(ClaimsUserLookup); ok {
		if !a.provisioning {
			return lookup.FindByAuthSubject(ctx, token.Subject)
		}
		return lookup.FindByClaims(ctx, claims)
	}
	return a.lookup.FindByAuthSubject(ctx, token.Subject)
}
