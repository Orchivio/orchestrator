package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/orchivio/orchestrator/internal/admin"
)

const bearerPrefix = "Bearer "

type principalKey struct{}

// PrincipalFrom returns the principal Authenticate stored in ctx.
func PrincipalFrom(ctx context.Context) (admin.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(admin.Principal)
	return p, ok
}

// Authenticate lets only requests with a valid admin bearer token (instance or
// tenant) through and stores their principal in the request context. Every
// failure is the same generic 401; the presented token is never echoed or logged.
func Authenticate(svc *admin.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if strings.HasPrefix(header, bearerPrefix) {
			tok, err := svc.Authenticate(r.Context(), strings.TrimSpace(header[len(bearerPrefix):]))
			if err != nil && !errors.Is(err, admin.ErrUnauthorized) {
				svc.Logger().Error("token lookup failed", "err", err)
				writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "The request could not be processed.")
				return
			}
			if p, ok := admin.PrincipalOf(tok); err == nil && ok {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "A valid bearer token is required.")
	})
}
