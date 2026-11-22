package httpapi_test

import (
	"bytes"
	"context"
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

type tokenStub struct{ admin.Store } // only token methods are used

func (tokenStub) TokenByHash(context.Context, []byte) (admin.Token, error) {
	return admin.Token{}, admin.ErrNotFound
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

func serve(logs io.Writer, header string) *httptest.ResponseRecorder {
	svc := admin.NewService(tokenStub{}, nil, time.Now, uuid.New, slog.New(slog.NewTextHandler(logs, nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/tenants", nil)
	if header != "" {
		req.Header.Set("Authorization", header)
	}
	httpapi.RequireInstanceAdmin(svc, okHandler).ServeHTTP(rec, req)
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
