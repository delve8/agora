package auth

import (
	"context"
	"errors"
	"testing"
)

type testValidator struct {
	token Token
	err   error
}

func (v testValidator) Validate(context.Context, string) (Token, error) { return v.token, v.err }

type testLookup struct {
	claims    ProvisionClaims
	principal Principal
	err       error
}

func (l *testLookup) FindByAuthSubject(context.Context, string) (Principal, error) {
	return l.principal, l.err
}
func (l *testLookup) FindByClaims(_ context.Context, claims ProvisionClaims) (Principal, error) {
	l.claims = claims
	return l.principal, l.err
}

func TestExtractBearer(t *testing.T) {
	for _, value := range []struct{ header, want string }{{"Bearer abc", "abc"}, {"bearer abc", "abc"}, {"Basic abc", ""}, {"Bearer", ""}, {"Bearer a b", ""}} {
		if got := ExtractBearer(value.header); got != value.want {
			t.Errorf("ExtractBearer(%q) = %q, want %q", value.header, got, value.want)
		}
	}
}

func TestAuthenticatorLocal(t *testing.T) {
	principal := Principal{UserID: "local", Provider: ProviderLocal}
	got, err := NewAuthenticator(ModeLocal, nil, nil, principal).Authenticate(context.Background(), "")
	if err != nil || got.UserID != "local" {
		t.Fatalf("local authentication = %+v, %v", got, err)
	}
}

func TestAuthenticatorProvisioningPolicy(t *testing.T) {
	lookup := &testLookup{principal: Principal{UserID: "user-1"}}
	validator := testValidator{token: Token{Subject: "subject-1", Provider: ProviderLogto, DisplayName: "User"}}
	got, err := NewAuthenticatorWithProvisioning(ModeLogto, validator, lookup, Principal{}, true).Authenticate(context.Background(), "token")
	if err != nil || got.UserID != "user-1" || lookup.claims.Subject != "subject-1" {
		t.Fatalf("provisioned authentication = %+v, %v, claims=%+v", got, err, lookup.claims)
	}
	lookup.claims = ProvisionClaims{}
	_, err = NewAuthenticatorWithProvisioning(ModeLogto, validator, lookup, Principal{}, false).Authenticate(context.Background(), "token")
	if err != nil || lookup.claims.Subject != "" {
		t.Fatalf("non-provisioning authentication should use existing identity: err=%v claims=%+v", err, lookup.claims)
	}
}

func TestAuthenticatorRejectsInvalidToken(t *testing.T) {
	_, err := NewAuthenticator(ModeLogto, testValidator{err: errors.New("bad")}, &testLookup{}, Principal{}).Authenticate(context.Background(), "token")
	if err == nil {
		t.Fatal("expected invalid token error")
	}
}
