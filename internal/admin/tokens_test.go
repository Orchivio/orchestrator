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
	return admin.NewService(st, nil, now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil))), st
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
