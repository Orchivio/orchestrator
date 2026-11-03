// Package httpapi holds the server's HTTP handlers.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/router"
)

// Event is one stored run event.
type Event struct {
	Type    string
	Payload json.RawMessage
	At      time.Time
}

// TimelineStore is the read side the timeline needs.
type TimelineStore interface {
	GetRun(ctx context.Context, id uuid.UUID) (*router.Run, error) // nil, nil if unknown
	ListEvents(ctx context.Context, runID uuid.UUID) ([]Event, error)
}

type runJSON struct {
	ID        string    `json:"id"`
	Tenant    string    `json:"tenant"`
	TicketRef string    `json:"ticketRef"`
	Kind      string    `json:"kind"`
	State     string    `json:"state"`
	Repos     []string  `json:"repos"`
	Branch    string    `json:"branch"`
	Result    string    `json:"result"`
	PRURLs    []string  `json:"prUrls"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type eventJSON struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
	At      time.Time       `json:"at"`
}

// NewTimeline serves GET /runs/{id} as {"run":{...},"events":[...]}.
// ponytail: ohne Auth, nur an localhost binden bis ORC-49 (OIDC)
func NewTimeline(store TimelineStore) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			http.Error(w, "invalid run id", http.StatusBadRequest)
			return
		}
		run, err := store.GetRun(r.Context(), id)
		if err == nil && run == nil {
			http.NotFound(w, r)
			return
		}
		var events []Event
		if err == nil {
			events, err = store.ListEvents(r.Context(), id)
		}
		if err != nil {
			slog.Error("timeline failed", "run", id, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out := struct {
			Run    runJSON     `json:"run"`
			Events []eventJSON `json:"events"`
		}{Run: toRunJSON(run), Events: make([]eventJSON, 0, len(events))}
		for _, e := range events {
			out.Events = append(out.Events, eventJSON(e))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	return mux
}

func toRunJSON(r *router.Run) runJSON {
	return runJSON{ID: r.ID.String(), Tenant: r.Tenant, TicketRef: r.TicketRef, Kind: r.Kind, State: r.State,
		Repos: r.Repos, Branch: r.Branch, Result: r.Result, PRURLs: r.PRURLs, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
