package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	tokenRandomBytes = 32
	tokenPrefixLen   = 12
	minTokenLen      = 36
	bootstrapName    = "bootstrap"
)

func hashToken(plaintext string) []byte {
	h := sha256.Sum256([]byte(plaintext))
	return h[:]
}

// IssueToken creates an instance admin token and returns its plaintext, which
// is not recoverable afterwards. ttl 0 means DefaultTokenTTL.
func (s *Service) IssueToken(ctx context.Context, name string, ttl time.Duration) (Token, string, error) {
	return s.issue(ctx, name, ttl, "")
}

// IssueTokenFor issues a token on behalf of p. An instance admin gets an
// instance token for tenant "" and a tenant admin token otherwise; a tenant
// admin may only issue for its own tenant ("" means its own).
func (s *Service) IssueTokenFor(ctx context.Context, p Principal, name string, ttl time.Duration, tenant string) (Token, string, error) {
	if !p.IsInstanceAdmin() {
		if tenant != "" && tenant != p.TenantSlug {
			return Token{}, "", ErrForbidden
		}
		tenant = p.TenantSlug
	}
	if tenant != "" {
		if err := s.requireTenant(ctx, tenant); errors.Is(err, ErrNotFound) {
			return Token{}, "", &ValidationError{[]config.FieldError{{Field: "tenant", Message: "unknown tenant"}}}
		} else if err != nil {
			return Token{}, "", err
		}
	}
	return s.issue(ctx, name, ttl, tenant)
}

// issue creates an admin token, instance-scoped when tenant is "".
func (s *Service) issue(ctx context.Context, name string, ttl time.Duration, tenant string) (Token, string, error) {
	if strings.TrimSpace(name) == "" {
		return Token{}, "", &ValidationError{[]config.FieldError{{Field: "name", Message: msgRequired}}}
	}
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	tok, err := s.createToken(ctx, name, plaintext, ttl, tenant)
	return tok, plaintext, err
}

func (s *Service) createToken(ctx context.Context, name, plaintext string, ttl time.Duration, tenant string) (Token, error) {
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	scope := ScopeInstance
	if tenant != "" {
		scope = ScopeTenant
	}
	now := s.now()
	return s.store.CreateToken(ctx, Token{ID: s.newID(), Name: name, Prefix: plaintext[:tokenPrefixLen],
		Scope: scope, TenantSlug: tenant, Role: RoleAdmin, CreatedAt: now, ExpiresAt: now.Add(ttl)}, hashToken(plaintext))
}

// Authenticate resolves a presented token; every failure is ErrUnauthorized.
func (s *Service) Authenticate(ctx context.Context, plaintext string) (Token, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return Token{}, ErrUnauthorized
	}
	tok, err := s.store.TokenByHash(ctx, hashToken(plaintext))
	if errors.Is(err, ErrNotFound) {
		return Token{}, ErrUnauthorized
	}
	if err != nil {
		return Token{}, fmt.Errorf("look up token: %w", err)
	}
	if tok.RevokedAt != nil || !s.now().Before(tok.ExpiresAt) {
		return Token{}, ErrUnauthorized
	}
	return tok, nil
}

// Bootstrap installs plaintext as the first instance admin token, only while
// the token table is completely empty. It reports whether it did.
func (s *Service) Bootstrap(ctx context.Context, plaintext string) (bool, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) || len(plaintext) < minTokenLen {
		return false, fmt.Errorf("bootstrap token must start with %q and have at least %d characters", TokenPrefix, minTokenLen)
	}
	n, err := s.store.CountTokens(ctx)
	if err != nil || n > 0 {
		return false, err
	}
	if _, err := s.createToken(ctx, bootstrapName, plaintext, 0, ""); err != nil {
		return false, err
	}
	return true, nil
}

// Logger returns the service logger, for middleware that reports infrastructure failures.
func (s *Service) Logger() *slog.Logger { return s.log }

func (s *Service) ListTokens(ctx context.Context) ([]Token, error) { return s.store.ListTokens(ctx) }

func (s *Service) RevokeToken(ctx context.Context, id uuid.UUID) error {
	return s.store.RevokeToken(ctx, id)
}

// ListTokensFor lists all tokens for an instance admin and only the tokens
// scoped to its own tenant for a tenant admin.
func (s *Service) ListTokensFor(ctx context.Context, p Principal) ([]Token, error) {
	all, err := s.store.ListTokens(ctx)
	if err != nil || p.IsInstanceAdmin() {
		return all, err
	}
	own := []Token{}
	for _, t := range all {
		if t.Scope == ScopeTenant && t.TenantSlug == p.TenantSlug {
			own = append(own, t)
		}
	}
	return own, nil
}

// RevokeTokenFor revokes a token p can see; any other id is ErrNotFound.
func (s *Service) RevokeTokenFor(ctx context.Context, p Principal, id uuid.UUID) error {
	if !p.IsInstanceAdmin() {
		// ponytail: scans the token list, add a GetToken query when tokens number in the thousands
		own, err := s.ListTokensFor(ctx, p)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(own, func(t Token) bool { return t.ID == id }) {
			return fmt.Errorf("token %s: %w", id, ErrNotFound)
		}
	}
	return s.RevokeToken(ctx, id)
}
