package httpapi

import (
	"encoding/json"
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
			if err == nil && tok.Scope == admin.ScopeInstance && tok.Role == admin.RoleAdmin {
				next.ServeHTTP(w, r)
				return
			}
		}
		writeUnauthorized(w)
	})
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": "Unauthorized",
		"status": http.StatusUnauthorized, "detail": "A valid bearer token is required."})
}
