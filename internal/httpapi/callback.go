package httpapi

import (
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/orchivio/orchestrator/internal/admin"
)

// callbackPage is self-contained: no external resources, text auto-escaped.
var callbackPage = template.Must(template.New("cb").Parse(`<!doctype html>
<html lang="de"><head><meta charset="utf-8"><title>Orchivio</title></head>
<body><p>{{.}}</p></body></html>
`))

const msgConnectFailed = "Verbindung fehlgeschlagen"

// NewCallback serves GET /oauth/callback/{adapter}, the provider's redirect
// target. It needs no token: the single-use state authorizes. Neither code,
// state nor credential appear in the page or the logs.
func NewCallback(svc *admin.Service, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := map[string]string{}
		for k, v := range r.URL.Query() {
			query[k] = v[0]
		}
		adapter := r.PathValue("adapter")
		c, err := svc.Callback(r.Context(), adapter, query)
		if err == nil {
			renderPage(w, log, http.StatusOK, "Verbunden: "+c.Label)
			return
		}
		status, msg := callbackFailure(err)
		log.Warn("oauth callback failed", "adapter", adapter, "status", status, "err", err)
		renderPage(w, log, status, msg)
	})
}

func callbackFailure(err error) (int, string) {
	var nc *admin.NotConnectedError
	switch {
	case errors.As(err, &nc):
		return http.StatusBadRequest, msgConnectFailed + ": " + nc.Reason
	case errors.Is(err, admin.ErrConflict):
		return http.StatusConflict, "Bereits mit einem anderen Mandanten verbunden"
	case errors.Is(err, admin.ErrInUse):
		return http.StatusConflict, msgConnectFailed + ": Ein anderer Workspace ist erst möglich, wenn der Mandant keine Projekte mehr hat"
	case errors.Is(err, admin.ErrUnavailable):
		return http.StatusServiceUnavailable, msgConnectFailed
	}
	return http.StatusBadRequest, msgConnectFailed
}

func renderPage(w http.ResponseWriter, log *slog.Logger, status int, msg string) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'")
	w.WriteHeader(status)
	if err := callbackPage.Execute(w, msg); err != nil {
		log.Warn("write callback page failed", "err", err)
	}
}
