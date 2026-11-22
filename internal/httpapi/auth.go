package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/orchivio/orchestrator/internal/admin"
)

const bearerPrefix = "Bearer "

// RequireInstanceAdmin lets only requests with a valid instance admin bearer
// token through. Every failure is the same generic 401; the presented token is
// never echoed or logged.
func RequireInstanceAdmin(svc *admin.Service, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		if strings.HasPrefix(header, bearerPrefix) {
			tok, err := svc.Authenticate(r.Context(), strings.TrimSpace(header[len(bearerPrefix):]))
			switch {
			case err == nil && tok.Scope == admin.ScopeInstance && tok.Role == admin.RoleAdmin:
				next.ServeHTTP(w, r)
				return
			case err != nil && !errors.Is(err, admin.ErrUnauthorized):
				svc.Logger().Error("token lookup failed", "err", err)
				writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "The request could not be processed.")
				return
			}
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, http.StatusUnauthorized, "Unauthorized", "A valid bearer token is required.")
	})
}

func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail})
}
