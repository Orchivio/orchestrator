// Package httpapi holds the server's HTTP handlers.
package httpapi

import (
	"encoding/json"
	"time"

	"github.com/orchivio/orchestrator/internal/router"
)

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

func toRunJSON(r router.Run) runJSON {
	return runJSON{ID: r.ID.String(), Tenant: r.Tenant, TicketRef: r.TicketRef, Kind: r.Kind, State: r.State,
		Repos: r.Repos, Branch: r.Branch, Result: r.Result, PRURLs: r.PRURLs, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}
