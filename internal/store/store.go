// Package store is the Postgres adapter for the router, orchestrator and
// timeline persistence ports.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/soemii/orchivio-server/internal/classifier"
	"github.com/soemii/orchivio-server/internal/httpapi"
	"github.com/soemii/orchivio-server/internal/orchestrator"
	"github.com/soemii/orchivio-server/internal/router"
	"github.com/soemii/orchivio-server/internal/store/db"
)

//go:embed migrations/*.sql
var migrations embed.FS

const agentAuthor = "Orchivio"

var (
	_ router.Store          = (*Store)(nil)
	_ orchestrator.Store    = (*Store)(nil)
	_ httpapi.TimelineStore = (*Store)(nil)
)

// Store persists conversations, inputs, runs and events in Postgres.
// ponytail: no InTx yet, the router does not need atomic multi-step writes; add with the first caller.
type Store struct {
	pool *pgxpool.Pool
	q    *db.Queries
	now  func() time.Time
}

// Open migrates the database to the latest schema and connects.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	if err := migrateUp(databaseURL); err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Store{pool: pool, q: db.New(pool), now: time.Now}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.pool.Close() }

func migrateUp(databaseURL string) error {
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("migration source: %w", err)
	}
	migrateURL, err := migrationURL(databaseURL)
	if err != nil {
		return err
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, migrateURL)
	if err != nil {
		return fmt.Errorf("migrate init: %w", err)
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrate up: %w", err)
	}
	return nil
}

// migrationURL rewrites a postgres URL to the "pgx5" scheme that
// golang-migrate's pgx/v5 driver registers. Errors never echo the URL (password).
func migrationURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return "", errors.New("invalid database URL: want postgres:// or postgresql://")
	}
	u.Scheme = "pgx5"
	return u.String(), nil
}

// orNil maps "no rows" to (nil, nil), the port convention for "not found".
func orNil[T any, R any](row T, err error, conv func(T) R) (*R, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r := conv(row)
	return &r, nil
}

func (s *Store) MarkInbound(ctx context.Context, source, externalID string) (bool, error) {
	n, err := s.q.MarkInbound(ctx, db.MarkInboundParams{Source: source, ExternalID: externalID, ReceivedAt: s.now()})
	return n == 1, err
}

func (s *Store) FindConversation(ctx context.Context, tenant, ticketRef string) (*router.Conversation, error) {
	row, err := s.q.FindConversation(ctx, db.FindConversationParams{TenantID: tenant, TicketRef: ticketRef})
	return orNil(row, err, toConversation)
}

func (s *Store) GetConversation(ctx context.Context, id uuid.UUID) (*router.Conversation, error) {
	row, err := s.q.GetConversation(ctx, id)
	return orNil(row, err, toConversation)
}

func (s *Store) CreateConversation(ctx context.Context, c router.Conversation) error {
	return s.q.CreateConversation(ctx, db.CreateConversationParams{
		ID: c.ID, TenantID: c.Tenant, Project: c.Project, TicketRef: c.TicketRef, CreatedAt: c.CreatedAt})
}

func (s *Store) AddInput(ctx context.Context, in router.Input) error {
	return s.q.AddInput(ctx, db.AddInputParams{
		ID: in.ID, TenantID: in.Tenant, ConversationID: in.ConversationID, ActorEmail: in.ActorEmail,
		Body: in.Body, Trigger: string(in.Trigger), RunID: in.RunID, CreatedAt: in.CreatedAt, FromAgent: in.FromAgent})
}

func (s *Store) OpenInputs(ctx context.Context, conversationID uuid.UUID) ([]router.Input, error) {
	rows, err := s.q.OpenInputs(ctx, conversationID)
	return mapRows(rows, err, toInput)
}

func (s *Store) ClaimInputs(ctx context.Context, conversationID, runID uuid.UUID) error {
	return s.q.ClaimInputs(ctx, db.ClaimInputsParams{ConversationID: conversationID, RunID: &runID})
}

func (s *Store) ActiveRun(ctx context.Context, conversationID uuid.UUID) (*router.Run, error) {
	row, err := s.q.ActiveRun(ctx, conversationID)
	return runOrNil(row, err)
}

func (s *Store) LastRun(ctx context.Context, conversationID uuid.UUID) (*router.Run, error) {
	row, err := s.q.LastRun(ctx, conversationID)
	return runOrNil(row, err)
}

func (s *Store) GetRun(ctx context.Context, id uuid.UUID) (*router.Run, error) {
	row, err := s.q.GetRun(ctx, id)
	return runOrNil(row, err)
}

func (s *Store) CreateRun(ctx context.Context, r router.Run) error {
	repos, err := jsonArray(r.Repos)
	if err != nil {
		return err
	}
	prURLs, err := jsonArray(r.PRURLs)
	if err != nil {
		return err
	}
	return s.q.CreateRun(ctx, db.CreateRunParams{
		ID: r.ID, TenantID: r.Tenant, ConversationID: r.ConversationID, Project: r.Project, TicketRef: r.TicketRef,
		Kind: r.Kind, State: r.State, Repos: repos, Branch: r.Branch, Prompt: r.Prompt, Result: r.Result,
		PrUrls: prURLs, SessionID: r.SessionID, LeaseOwner: r.LeaseOwner, LeaseUntil: r.LeaseUntil,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
}

func (s *Store) UpdateRun(ctx context.Context, r router.Run) error {
	repos, err := jsonArray(r.Repos)
	if err != nil {
		return err
	}
	prURLs, err := jsonArray(r.PRURLs)
	if err != nil {
		return err
	}
	n, err := s.q.UpdateRun(ctx, db.UpdateRunParams{
		ID: r.ID, State: r.State, Repos: repos, Branch: r.Branch, Prompt: r.Prompt, Result: r.Result,
		PrUrls: prURLs, Kind: r.Kind, SessionID: r.SessionID, LeaseOwner: r.LeaseOwner, LeaseUntil: r.LeaseUntil,
		UpdatedAt: r.UpdatedAt})
	if err == nil && n == 0 {
		return fmt.Errorf("update run %s: not found", r.ID)
	}
	return err
}

// LeaseNext hands the oldest queued (or lease-expired running) run to a worker.
func (s *Store) LeaseNext(ctx context.Context, workerID string, until time.Time) (*router.Run, error) {
	row, err := s.q.LeaseNext(ctx, db.LeaseNextParams{LeaseOwner: workerID, LeaseUntil: &until, UpdatedAt: s.now()})
	return runOrNil(row, err)
}

// History merges inputs (human comments and agent questions) and final agent
// results chronologically.
func (s *Store) History(ctx context.Context, conversationID uuid.UUID) ([]router.HistoryEntry, error) {
	inputs, err := s.q.ListInputs(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	runs, err := s.q.ListFinishedRuns(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	entries := make([]router.HistoryEntry, 0, len(inputs)+len(runs))
	for _, in := range inputs {
		switch {
		case in.FromAgent:
			entries = append(entries, router.HistoryEntry{At: in.CreatedAt.UTC(), Author: agentAuthor, Text: in.Body, FromAgent: true})
		case in.Body != "":
			entries = append(entries, router.HistoryEntry{At: in.CreatedAt.UTC(), Author: in.ActorEmail, Text: in.Body})
		}
	}
	for _, r := range runs {
		entries = append(entries, router.HistoryEntry{At: r.UpdatedAt.UTC(), Author: agentAuthor, Text: r.Result, FromAgent: true})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	return entries, nil
}

// AppendEvent stores json.RawMessage payloads as-is and marshals anything else.
func (s *Store) AppendEvent(ctx context.Context, tenant string, runID uuid.UUID, typ string, payload any) error {
	raw, ok := payload.(json.RawMessage)
	if !ok {
		var err error
		if raw, err = json.Marshal(payload); err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
	}
	return s.q.AppendEvent(ctx, db.AppendEventParams{TenantID: tenant, RunID: runID, Type: typ, Payload: raw, At: s.now()})
}

func (s *Store) ListEvents(ctx context.Context, runID uuid.UUID) ([]httpapi.Event, error) {
	rows, err := s.q.ListEvents(ctx, runID)
	return mapRows(rows, err, func(e db.Event) httpapi.Event {
		return httpapi.Event{Type: e.Type, Payload: e.Payload, At: e.At.UTC()}
	})
}

func mapRows[T, R any](rows []T, err error, conv func(T) R) ([]R, error) {
	if err != nil {
		return nil, err
	}
	out := make([]R, len(rows))
	for i, r := range rows {
		out[i] = conv(r)
	}
	return out, nil
}

func runOrNil(row db.Run, err error) (*router.Run, error) {
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r, err := toRun(row)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func jsonArray(items []string) ([]byte, error) {
	if items == nil {
		items = []string{}
	}
	return json.Marshal(items)
}

func toConversation(c db.Conversation) router.Conversation {
	return router.Conversation{ID: c.ID, Tenant: c.TenantID, Project: c.Project, TicketRef: c.TicketRef, CreatedAt: c.CreatedAt.UTC()}
}

func toInput(i db.Input) router.Input {
	return router.Input{ID: i.ID, Tenant: i.TenantID, ConversationID: i.ConversationID, ActorEmail: i.ActorEmail,
		Body: i.Body, Trigger: classifier.Trigger(i.Trigger), RunID: i.RunID, CreatedAt: i.CreatedAt.UTC(), FromAgent: i.FromAgent}
}

func toRun(r db.Run) (router.Run, error) {
	repos, err := stringArray(r.Repos)
	if err != nil {
		return router.Run{}, fmt.Errorf("run %s repos: %w", r.ID, err)
	}
	prURLs, err := stringArray(r.PrUrls)
	if err != nil {
		return router.Run{}, fmt.Errorf("run %s pr_urls: %w", r.ID, err)
	}
	return router.Run{ID: r.ID, Tenant: r.TenantID, ConversationID: r.ConversationID, Project: r.Project,
		TicketRef: r.TicketRef, Kind: r.Kind, State: r.State, Repos: repos, Branch: r.Branch,
		Prompt: r.Prompt, Result: r.Result, PRURLs: prURLs, SessionID: r.SessionID,
		LeaseOwner: r.LeaseOwner, LeaseUntil: utcPtr(r.LeaseUntil), CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC()}, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func stringArray(raw []byte) ([]string, error) {
	out := []string{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
