package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/httpapi"
)

type tokenStub struct { // only token lookup is used
	admin.Store
	tok admin.Token
	err error
}

func (s tokenStub) TokenByHash(context.Context, []byte) (admin.Token, error) {
	if s.err != nil {
		return admin.Token{}, s.err
	}
	return s.tok, nil
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

func serve(logs io.Writer, header string) *httptest.ResponseRecorder {
	return serveWith(tokenStub{err: admin.ErrNotFound}, logs, header)
}

func serveWith(st tokenStub, logs io.Writer, header string) *httptest.ResponseRecorder {
	svc := admin.NewService(st, nil, nil, nil, "", time.Now, uuid.New, slog.New(slog.NewTextHandler(logs, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/tenants", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	httpapi.Authenticate(svc, okHandler).ServeHTTP(rec, req)
	return rec
}

func TestAuthNeverLogsToken(t *testing.T) {
	var logs bytes.Buffer
	rec := serve(&logs, "Bearer orv_wrongwrongwrongwrongwrongwrongwrong")
	if rec.Code != 401 || strings.Contains(rec.Body.String()+logs.String(), "orv_wrong") {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestAuthRequiresBearer(t *testing.T) {
	for _, h := range []string{"", "Basic x", "Bearer ", "Bearer"} {
		rec := serve(io.Discard, h)
		if rec.Code != 401 || rec.Header().Get("Content-Type") != "application/problem+json" ||
			!strings.Contains(rec.Body.String(), `"status":401`) {
			t.Fatalf("%q: code=%d ct=%q body=%s", h, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	}
}

const validToken = "Bearer orv_validvalidvalidvalidvalidvalidvalid"

func tokenOf(scope, role string) admin.Token {
	return admin.Token{ID: uuid.New(), Scope: scope, Role: role, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestValidInstanceAdminTokenPasses(t *testing.T) {
	rec := serveWith(tokenStub{tok: tokenOf(admin.ScopeInstance, admin.RoleAdmin)}, io.Discard, validToken)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestOtherScopeOrRoleRejected(t *testing.T) {
	for _, tok := range []admin.Token{tokenOf(admin.ScopeTenant, admin.RoleAdmin), tokenOf(admin.ScopeInstance, "viewer")} {
		if rec := serveWith(tokenStub{tok: tok}, io.Discard, validToken); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%+v: code = %d", tok, rec.Code)
		}
	}
}

func TestStoreOutageIs500AndLoggedWithoutToken(t *testing.T) {
	var logs bytes.Buffer
	rec := serveWith(tokenStub{err: errors.New("connection refused")}, &logs, validToken)
	all := rec.Body.String() + logs.String()
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("Content-Type") != "application/problem+json" ||
		strings.Contains(rec.Body.String(), "connection refused") || !strings.Contains(logs.String(), "token lookup failed") ||
		strings.Contains(all, "orv_valid") {
		t.Fatalf("code=%d body=%s logs=%s", rec.Code, rec.Body, logs.String())
	}
}

func TestTenantAdminTokenPassesWithPrincipal(t *testing.T) {
	tok := tokenOf(admin.ScopeTenant, admin.RoleAdmin)
	tok.TenantSlug = "acme"
	svc := admin.NewService(tokenStub{tok: tok}, nil, nil, nil, "", time.Now, uuid.New, slog.New(slog.DiscardHandler))
	var got admin.Principal
	h := httpapi.Authenticate(svc, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = httpapi.PrincipalFrom(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest("GET", "/api/v1/tenants", nil)
	req.Header.Set("Authorization", validToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	want := admin.Principal{Scope: admin.ScopeTenant, TenantSlug: "acme", Role: admin.RoleAdmin}
	if rec.Code != http.StatusNoContent || got != want {
		t.Fatalf("code = %d principal = %+v", rec.Code, got)
	}
}
