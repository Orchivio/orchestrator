package httpapi

import (
	"net/http"
	"time"

	"github.com/orchivio/orchestrator/internal/admin"
)

// connectionDTO never carries the credential.
type connectionDTO struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Adapter   string    `json:"adapter"`
	Label     string    `json:"label"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

func connectionOut(c admin.Connection) connectionDTO {
	return connectionDTO{c.ID.String(), c.Kind, c.Adapter, c.Label, c.Status, c.CreatedAt}
}

func (a *adminAPI) authorize(w http.ResponseWriter, r *http.Request) {
	u, err := a.svc.Authorize(r.Context(), r.PathValue("slug"), r.PathValue("adapter"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, 0, map[string]string{"url": u})
}

func (a *adminAPI) listConnections(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListConnections(r.Context(), r.PathValue("slug"))
	list(a, w, items, err, connectionOut)
}

func (a *adminAPI) getConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	c, err := a.svc.GetConnection(r.Context(), r.PathValue("slug"), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, 0, connectionOut(c))
}

func (a *adminAPI) deleteConnection(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r); ok {
		a.noContent(w, a.svc.DeleteConnection(r.Context(), r.PathValue("slug"), id))
	}
}
