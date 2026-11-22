package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
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
	if strings.TrimSpace(name) == "" {
		return Token{}, "", &ValidationError{[]config.FieldError{{Field: "name", Message: msgRequired}}}
	}
	buf := make([]byte, tokenRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return Token{}, "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	tok, err := s.createToken(ctx, name, plaintext, ttl)
	return tok, plaintext, err
}

func (s *Service) createToken(ctx context.Context, name, plaintext string, ttl time.Duration) (Token, error) {
	if ttl == 0 {
		ttl = DefaultTokenTTL
	}
	now := s.now()
	return s.store.CreateToken(ctx, Token{ID: s.newID(), Name: name, Prefix: plaintext[:tokenPrefixLen],
		Scope: ScopeInstance, Role: RoleAdmin, CreatedAt: now, ExpiresAt: now.Add(ttl)}, hashToken(plaintext))
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
	if _, err := s.createToken(ctx, bootstrapName, plaintext, 0); err != nil {
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
