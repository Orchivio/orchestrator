// Package router turns normalized tracker events into conversations,
// role-checked inputs and classified runs.
package router

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/classifier"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

// Run states (strings in DB and proto).
const (
	StateClassifying     = "classifying"
	StateQueued          = "queued"
	StateRunning         = "running"
	StateWaitingForInput = "waiting_for_input"
	StatePushing         = "pushing"
	StateSucceeded       = "succeeded"
	StateFailed          = "failed"
	StateCancelled       = "cancelled"
)

// Conversation is the thread of inputs and runs of one ticket. Project holds
// the tracker key of the project.
type Conversation struct {
	ID        uuid.UUID
	Tenant    string
	Project   string
	TicketRef string
	CreatedAt time.Time
}

// Input is one message of the conversation: a human comment, or with FromAgent
// the agent's question that put its run into waiting_for_input. RunID is nil
// while the input is open.
type Input struct {
	ID             uuid.UUID
	Tenant         string
	ConversationID uuid.UUID
	// ActorEmail holds the identity of the actor that matched a role: an e-mail
	// address or "<system>:<id>". The column keeps its historic name.
	ActorEmail string
	// Actors are all identities of the actor; empty on rows from before they
	// were stored, see Identities.
	Actors    []string
	Body      string
	Trigger   classifier.Trigger
	RunID     *uuid.UUID
	CreatedAt time.Time
	FromAgent bool
}

// Identities returns all identities of the input's actor, falling back to
// ActorEmail for rows that stored none.
func (in Input) Identities() []string {
	if len(in.Actors) > 0 {
		return in.Actors
	}
	if in.ActorEmail == "" {
		return nil
	}
	return []string{in.ActorEmail}
}

// Run is one agent execution. Project holds the tracker key of the project.
type Run struct {
	ID             uuid.UUID
	Tenant         string
	ConversationID uuid.UUID
	Project        string
	ProjectID      uuid.UUID // uuid.Nil = run predates managed projects
	TicketRef      string
	Kind           string
	State          string
	Repos          []string
	Branch         string
	Prompt         string
	Result         string
	PRURLs         []string
	SessionID      string
	LeaseOwner     string
	LeaseUntil     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// HistoryEntry is one line of the conversation history shown to the agent.
type HistoryEntry struct {
	At        time.Time
	Author    string
	Text      string
	FromAgent bool
}

// Store is the persistence port of the router.
type Store interface {
	MarkInbound(ctx context.Context, tenant, source, externalID string) (bool, error)
	FindConversation(ctx context.Context, tenant, ticketRef string) (*Conversation, error) // nil, nil if none
	GetConversation(ctx context.Context, id uuid.UUID) (*Conversation, error)
	CreateConversation(ctx context.Context, c Conversation) error
	AddInput(ctx context.Context, in Input) error
	OpenInputs(ctx context.Context, conversationID uuid.UUID) ([]Input, error) // inputs without run, oldest first
	ActiveRun(ctx context.Context, conversationID uuid.UUID) (*Run, error)     // classifying|queued|running|pushing|waiting_for_input
	LastRun(ctx context.Context, conversationID uuid.UUID) (*Run, error)
	CreateRun(ctx context.Context, r Run) error
	UpdateRun(ctx context.Context, r Run) error
	ClaimInputs(ctx context.Context, conversationID, runID uuid.UUID) error
	History(ctx context.Context, conversationID uuid.UUID) ([]HistoryEntry, error)
	AppendEvent(ctx context.Context, tenant string, runID uuid.UUID, typ string, payload any) error
}

// Tracker reads tickets from the tenant's tracker adapter.
type Tracker interface {
	// AdapterName is the adapter_name the tracker adapter reports; it names the
	// system in "<system>:<id>" role entries.
	AdapterName(ctx context.Context, tenant string) (string, error)
	GetTicket(ctx context.Context, tenant, ref string) (*pluginv1.Ticket, error)
}

// Reporter writes milestones and notices back to the ticket.
type Reporter interface {
	Milestone(ctx context.Context, run Run, milestone, text string) error
	Notice(ctx context.Context, tenant, ticketRef, text string) error
}
