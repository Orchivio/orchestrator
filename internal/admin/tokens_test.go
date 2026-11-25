package admin_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
)

func tokenService(now func() time.Time) (*admin.Service, *memStore) {
	st := newMemStore()
	return admin.NewService(st, nil, nil, nil, "", now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

func TestIssuedTokenAuthenticatesUntilRevoked(t *testing.T) {
	svc, _ := tokenService(time.Now)
	tok, plain, err := svc.IssueToken(ctx, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Authenticate(ctx, plain)
	if err != nil || got.Scope != admin.ScopeInstance || got.Role != admin.RoleAdmin || got.ID != tok.ID {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := svc.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Authenticate(ctx, plain); !errors.Is(err, admin.ErrUnauthorized) {
		t.Fatalf("revoked token: %v", err)
	}
}

func TestExpiredTokenRejected(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, _ := tokenService(func() time.Time { return now })
	tok, plain, err := svc.IssueToken(ctx, "ci", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	now = tok.ExpiresAt
	if _, err := svc.Authenticate(ctx, plain); !errors.Is(err, admin.ErrUnauthorized) {
		t.Fatalf("expired token: %v", err)
	}
}

func TestIssueTokenRequiresName(t *testing.T) {
	svc, _ := tokenService(time.Now)
	var ve *admin.ValidationError
	if _, _, err := svc.IssueToken(ctx, " ", 0); !errors.As(err, &ve) || !hasField(ve, "name") {
		t.Fatalf("err = %v", err)
	}
}

func TestPlaintextNeverStored(t *testing.T) {
	svc, st := tokenService(time.Now)
	_, plain, err := svc.IssueToken(ctx, "ci", 0)
	if err != nil {
		t.Fatal(err)
	}
	for hash := range st.hashes {
		if bytes.Contains([]byte(hash), []byte(plain)) || hash == plain {
			t.Fatal("plaintext stored")
		}
	}
	list, _ := svc.ListTokens(ctx)
	if len(list) != 1 || len(list[0].Prefix) != 12 || list[0].Prefix != plain[:12] {
		t.Fatalf("list = %+v", list)
	}
}

func TestBootstrapOnlyOnEmptyTable(t *testing.T) {
	svc, _ := tokenService(time.Now)
	tok1 := admin.TokenPrefix + "bootstrapbootstrapbootstrapbootstrap1"
	tok2 := admin.TokenPrefix + "bootstrapbootstrapbootstrapbootstrap2"
	ok, err := svc.Bootstrap(ctx, tok1)
	list, _ := svc.ListTokens(ctx)
	_ = svc.RevokeToken(ctx, list[0].ID)
	ok2, err2 := svc.Bootstrap(ctx, tok2)
	if err != nil || err2 != nil || !ok || ok2 {
		t.Fatalf("ok=%v err=%v ok2=%v err2=%v", ok, err, ok2, err2)
	}
	if _, err := svc.Authenticate(ctx, tok2); !errors.Is(err, admin.ErrUnauthorized) {
		t.Fatal("tok2 must not work")
	}
}

func TestBootstrapRejectsMalformedToken(t *testing.T) {
	svc, _ := tokenService(time.Now)
	for _, bad := range []string{"secret", "orv_short", "xyz_" + string(bytes.Repeat([]byte("a"), 40))} {
		if ok, err := svc.Bootstrap(ctx, bad); err == nil || ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

var (
	instanceAdmin = admin.Principal{Scope: admin.ScopeInstance, Role: admin.RoleAdmin}
	acmeAdmin     = admin.Principal{Scope: admin.ScopeTenant, TenantSlug: "acme", Role: admin.RoleAdmin}
)

func tokenServiceWithTenants(t *testing.T, slugs ...string) *admin.Service {
	t.Helper()
	svc, st := tokenService(time.Now)
	for _, s := range slugs {
		if _, err := st.CreateTenant(ctx, admin.Tenant{Slug: s}); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

func TestInstanceAdminIssuesTenantScopedToken(t *testing.T) {
	svc := tokenServiceWithTenants(t, "acme")
	tok, plain, err := svc.IssueTokenFor(ctx, instanceAdmin, "acme-ci", 0, "acme")
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Authenticate(ctx, plain)
	if err != nil || got.ID != tok.ID || got.Scope != admin.ScopeTenant || got.TenantSlug != "acme" || got.Role != admin.RoleAdmin {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestInstanceAdminWithoutTenantIssuesInstanceToken(t *testing.T) {
	svc := tokenServiceWithTenants(t)
	tok, _, err := svc.IssueTokenFor(ctx, instanceAdmin, "ci", 0, "")
	if err != nil || tok.Scope != admin.ScopeInstance || tok.TenantSlug != "" {
		t.Fatalf("tok %+v err %v", tok, err)
	}
}

func TestIssueTokenForUnknownTenantIsInvalid(t *testing.T) {
	svc := tokenServiceWithTenants(t)
	var ve *admin.ValidationError
	if _, _, err := svc.IssueTokenFor(ctx, instanceAdmin, "ci", 0, "nope"); !errors.As(err, &ve) || !hasField(ve, "tenant") {
		t.Fatalf("err = %v", err)
	}
}

func TestTenantAdminIssuesOnlyForOwnTenant(t *testing.T) {
	svc := tokenServiceWithTenants(t, "acme", "other")
	if _, _, err := svc.IssueTokenFor(ctx, acmeAdmin, "x", 0, "other"); !errors.Is(err, admin.ErrForbidden) {
		t.Fatalf("other tenant: %v", err)
	}
	tok, _, err := svc.IssueTokenFor(ctx, acmeAdmin, "x", 0, "")
	if err != nil || tok.Scope != admin.ScopeTenant || tok.TenantSlug != "acme" {
		t.Fatalf("default tenant: tok %+v err %v", tok, err)
	}
}

func TestTenantAdminSeesAndRevokesOnlyOwnTenantTokens(t *testing.T) {
	svc := tokenServiceWithTenants(t, "acme", "other")
	inst, _, _ := svc.IssueTokenFor(ctx, instanceAdmin, "inst", 0, "")
	foreign, _, _ := svc.IssueTokenFor(ctx, instanceAdmin, "other", 0, "other")
	own, _, _ := svc.IssueTokenFor(ctx, instanceAdmin, "own", 0, "acme")
	list, err := svc.ListTokensFor(ctx, acmeAdmin)
	if err != nil || len(list) != 1 || list[0].ID != own.ID {
		t.Fatalf("list %+v err %v", list, err)
	}
	for _, id := range []uuid.UUID{inst.ID, foreign.ID} {
		if err := svc.RevokeTokenFor(ctx, acmeAdmin, id); !errors.Is(err, admin.ErrNotFound) {
			t.Fatalf("revoke %s: %v", id, err)
		}
	}
	if err := svc.RevokeTokenFor(ctx, acmeAdmin, own.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := svc.ListTokensFor(ctx, instanceAdmin)
	if len(all) != 3 {
		t.Fatalf("instance admin sees %d tokens", len(all))
	}
}

func TestPrincipalWithoutRightsIsForbiddenEverywhere(t *testing.T) {
	svc := tokenServiceWithTenants(t, "acme")
	tok, _, err := svc.IssueTokenFor(ctx, instanceAdmin, "own", 0, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []admin.Principal{
		{},
		{Scope: admin.ScopeTenant, Role: admin.RoleAdmin},
		{Scope: admin.ScopeTenant, TenantSlug: "acme", Role: "viewer"},
	} {
		calls := map[string]func() error{
			"IssueTokenFor":   func() error { _, _, err := svc.IssueTokenFor(ctx, p, "x", 0, ""); return err },
			"ListTokensFor":   func() error { _, err := svc.ListTokensFor(ctx, p); return err },
			"RevokeTokenFor":  func() error { return svc.RevokeTokenFor(ctx, p, tok.ID) },
			"ListTenantsFor":  func() error { _, err := svc.ListTenantsFor(ctx, p); return err },
			"CreateTenantFor": func() error { _, err := svc.CreateTenantFor(ctx, p, admin.Tenant{Slug: "new"}); return err },
			"DeleteTenantFor": func() error { return svc.DeleteTenantFor(ctx, p, "acme", 0) },
			"UpdateTenantFor": func() error { _, err := svc.UpdateTenantFor(ctx, p, admin.Tenant{Slug: "acme"}, 0); return err },
			"SeesTenant":      func() error { return p.SeesTenant(p.TenantSlug) },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, admin.ErrForbidden) {
				t.Errorf("%+v %s: err = %v, want ErrForbidden", p, name, err)
			}
		}
	}
}
