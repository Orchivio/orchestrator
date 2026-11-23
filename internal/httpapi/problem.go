package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/orchivio/orchestrator/internal/config"
)

const problemContentType = "application/problem+json"

// writeProblem sends an RFC 9457 problem document.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	writeProblemFields(w, status, title, detail, nil)
}

// writeProblemFields adds the per-field errors of a validation problem.
func writeProblemFields(w http.ResponseWriter, status int, title, detail string, fields []config.FieldError) {
	doc := map[string]any{"type": "about:blank", "title": title, "status": status, "detail": detail}
	if fields != nil {
		doc["errors"] = fields
	}
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(doc)
}
