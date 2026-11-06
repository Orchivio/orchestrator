package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/classifier"
	"github.com/soemii/orchivio-server/internal/config"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
)

const (
	NoticeMissingRole = "Dafür fehlt dir die Rolle in Orchivio. Bitte wende dich an die Verantwortlichen des Projekts."
	TextWhichRepo     = "Welches Repo ist gemeint? Setze bitte ein passendes Label und antworte hier."

	eventRunState      = "run.state"
	eventRunClassified = "run.classified"
	branchPrefix       = "orchivio/"
	shortIDLen         = 8
)

// Router routes tracker events to conversations and runs.
type Router struct {
	// ponytail: global lock, per-conversation lock or DB unique index if throughput matters
	mu       sync.Mutex
	cfg      *config.Sync
	store    Store
	tracker  Tracker
	reporter Reporter
	now      func() time.Time
	newID    func() uuid.UUID
}

func New(cfg *config.Sync, s Store, t Tracker, r Reporter, now func() time.Time, newID func() uuid.UUID) *Router {
	return &Router{cfg: cfg, store: s, tracker: t, reporter: r, now: now, newID: newID}
}

// Handle applies one tracker event. Order of the steps is the specification.
func (r *Router) Handle(ctx context.Context, tenant string, ev *pluginv1.TrackerEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.cfg.Current()
	tn, ok := cfg.Tenants[tenant]
	if !ok {
		return nil
	}
	fresh, err := r.store.MarkInbound(ctx, tn.Tracker.Name, ev.GetExternalId())
	if err != nil {
		return fmt.Errorf("mark inbound %s: %w", ev.GetTicketRef(), err)
	}
	if !fresh {
		return nil
	}
	_, project, ok := cfg.ProjectFor(tenant, ev.GetProjectKey())
	if !ok {
		return nil
	}
	conv, err := r.store.FindConversation(ctx, tenant, ev.GetTicketRef())
	if err != nil {
		return fmt.Errorf("find conversation %s: %w", ev.GetTicketRef(), err)
	}
	trigger, relevant := triggerFor(ev, conv != nil)
	if !relevant {
		return nil
	}
	email := ev.GetActor().GetEmail()
	if !classifier.HasAnyRole(project, email) {
		if trigger == classifier.TriggerFollowup {
			slog.Debug("ignoring follow-up comment from actor without role", "ticket", ev.GetTicketRef())
			return nil
		}
		return r.reporter.Notice(ctx, tenant, ev.GetTicketRef(), NoticeMissingRole)
	}
	if conv == nil {
		conv = &Conversation{ID: r.newID(), Tenant: tenant, Project: project.TrackerKey, TicketRef: ev.GetTicketRef(), CreatedAt: r.now()}
		if err := r.store.CreateConversation(ctx, *conv); err != nil {
			return fmt.Errorf("create conversation %s: %w", ev.GetTicketRef(), err)
		}
	}
	in := Input{ID: r.newID(), Tenant: tenant, ConversationID: conv.ID, ActorEmail: email, Trigger: trigger, CreatedAt: r.now()}
	if ev.GetKind() == pluginv1.TrackerEvent_COMMENTED {
		in.Body = ev.GetBody()
	}
	if err := r.store.AddInput(ctx, in); err != nil {
		return fmt.Errorf("add input %s: %w", ev.GetTicketRef(), err)
	}
	return r.start(ctx, tn, project, conv, trigger, email)
}

func triggerFor(ev *pluginv1.TrackerEvent, hasConversation bool) (classifier.Trigger, bool) {
	switch {
	case ev.GetKind() == pluginv1.TrackerEvent_ASSIGNED:
		return classifier.TriggerAssigned, true
	case ev.GetKind() == pluginv1.TrackerEvent_COMMENTED && ev.GetMentionsBot():
		return classifier.TriggerMentioned, true
	case ev.GetKind() == pluginv1.TrackerEvent_COMMENTED && hasConversation:
		return classifier.TriggerFollowup, true
	}
	return "", false
}

// StartFollowUp starts a run for inputs that arrived while a run was active.
func (r *Router) StartFollowUp(ctx context.Context, conversationID uuid.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	open, err := r.store.OpenInputs(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("open inputs %s: %w", conversationID, err)
	}
	if len(open) == 0 {
		return nil
	}
	conv, err := r.store.GetConversation(ctx, conversationID)
	if err != nil {
		return fmt.Errorf("get conversation %s: %w", conversationID, err)
	}
	if conv == nil {
		return fmt.Errorf("get conversation %s: not found", conversationID)
	}
	tn, project, ok := r.cfg.Current().ProjectFor(conv.Tenant, conv.Project)
	if !ok {
		return fmt.Errorf("follow-up %s: project %s not configured", conv.TicketRef, conv.Project)
	}
	return r.start(ctx, tn, project, conv, classifier.TriggerFollowup, open[len(open)-1].ActorEmail)
}

// start defers while a run is busy, resumes a waiting run or creates a new one.
func (r *Router) start(ctx context.Context, tn config.Tenant, p config.Project, conv *Conversation, trigger classifier.Trigger, email string) error {
	active, err := r.store.ActiveRun(ctx, conv.ID)
	if err != nil {
		return fmt.Errorf("active run %s: %w", conv.TicketRef, err)
	}
	if active != nil && active.State != StateWaitingForInput {
		return nil // input stays open, follow-up run later
	}
	last, err := r.store.LastRun(ctx, conv.ID) // before CreateRun, or it would return the new run
	if err != nil {
		return fmt.Errorf("last run %s: %w", conv.TicketRef, err)
	}
	run := active
	if run == nil {
		now := r.now()
		run = &Run{ID: r.newID(), Tenant: conv.Tenant, ConversationID: conv.ID, Project: p.TrackerKey,
			TicketRef: conv.TicketRef, State: StateClassifying, CreatedAt: now, UpdatedAt: now}
		if err := r.store.CreateRun(ctx, *run); err != nil {
			return fmt.Errorf("create run %s: %w", conv.TicketRef, err)
		}
		err = nil
		err = r.store.AppendEvent(ctx, conv.Tenant, run.ID, eventRunState, map[string]string{"state": StateClassifying})
	} else {
		err = r.setState(ctx, run, StateClassifying)
	}
	if err == nil {
		err = r.classify(ctx, run, last, p, trigger, email)
	}
	if err != nil {
		return r.fail(ctx, run, err)
	}
	return nil
}

// fail parks a run in failed so a stuck classifying run cannot strand later inputs.
func (r *Router) fail(ctx context.Context, run *Run, cause error) error {
	if err := r.setState(ctx, run, StateFailed); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func (r *Router) classify(ctx context.Context, run, last *Run, p config.Project, trigger classifier.Trigger, email string) error {
	ref := run.TicketRef
	ticket, err := r.tracker.GetTicket(ctx, run.Tenant, ref)
	if err != nil {
		return fmt.Errorf("get ticket %s: %w", ref, err)
	}
	open, err := r.store.OpenInputs(ctx, run.ConversationID)
	if err != nil {
		return fmt.Errorf("open inputs %s: %w", ref, err)
	}
	others := make([]string, 0, len(open))
	for _, in := range open {
		others = append(others, in.ActorEmail)
	}
	previousKind := ""
	if last != nil {
		previousKind = last.Kind
		if trigger == classifier.TriggerMentioned {
			trigger = classifier.TriggerFollowup // a mention continues the conversation's kind; labels still win
		}
	}
	d := classifier.Classify(p, classifier.Input{Trigger: trigger, PreviousKind: previousKind, Labels: ticket.GetLabels(),
		ActorEmail: email, OtherActors: others})
	run.Kind, run.Repos = d.Kind, d.Repos
	payload := map[string]any{"kind": d.Kind, "repos": d.Repos, "reason": d.Reason, "notes": d.Notes}
	if err := r.store.AppendEvent(ctx, run.Tenant, run.ID, eventRunClassified, payload); err != nil {
		return fmt.Errorf("record classification %s: %w", ref, err)
	}
	if len(d.Notes) > 0 {
		if err := r.reporter.Milestone(ctx, *run, config.MilestoneClassifier, strings.Join(d.Notes, " ")); err != nil {
			return fmt.Errorf("milestone classifier %s: %w", ref, err)
		}
	}
	if d.NeedsRepo {
		if err := r.setState(ctx, run, StateWaitingForInput); err != nil {
			return err
		}
		return r.milestone(ctx, run, config.MilestoneQuestion, TextWhichRepo)
	}
	return r.queue(ctx, run, ticket, last)
}

func (r *Router) queue(ctx context.Context, run *Run, ticket *pluginv1.Ticket, last *Run) error {
	ref := run.TicketRef
	if err := r.store.ClaimInputs(ctx, run.ConversationID, run.ID); err != nil {
		return fmt.Errorf("claim inputs %s: %w", ref, err)
	}
	history, err := r.store.History(ctx, run.ConversationID)
	if err != nil {
		return fmt.Errorf("history %s: %w", ref, err)
	}
	run.Prompt = BuildPrompt(ticket, history, run.Kind)
	if run.Kind == classifier.KindTask {
		run.Branch = r.branchFor(run, last)
	} else {
		run.Branch = ""
	}
	if err := r.setState(ctx, run, StateQueued); err != nil {
		return err
	}
	text := fmt.Sprintf("Gestartet als Frage in %s.", strings.Join(run.Repos, ", "))
	if run.Kind == classifier.KindTask {
		text = fmt.Sprintf("Gestartet als Aufgabe in %s.", strings.Join(run.Repos, ", "))
	}
	return r.milestone(ctx, run, config.MilestoneStarted, text)
}

// branchFor keeps the branch of a resumed run, reuses the previous task's branch,
// or derives a fresh one.
func (r *Router) branchFor(run, last *Run) string {
	if run.Branch != "" {
		return run.Branch
	}
	if last != nil && last.ID != run.ID && last.Kind == classifier.KindTask && last.Branch != "" {
		return last.Branch
	}
	return branchPrefix + strings.ToLower(run.TicketRef) + "-" + run.ID.String()[:shortIDLen]
}

func (r *Router) setState(ctx context.Context, run *Run, state string) error {
	run.State = state
	run.UpdatedAt = r.now()
	if err := r.store.UpdateRun(ctx, *run); err != nil {
		return fmt.Errorf("update run %s: %w", run.TicketRef, err)
	}
	if err := r.store.AppendEvent(ctx, run.Tenant, run.ID, eventRunState, map[string]string{"state": state}); err != nil {
		return fmt.Errorf("record state %s: %w", run.TicketRef, err)
	}
	return nil
}

func (r *Router) milestone(ctx context.Context, run *Run, milestone, text string) error {
	if err := r.reporter.Milestone(ctx, *run, milestone, text); err != nil {
		return fmt.Errorf("milestone %s %s: %w", milestone, run.TicketRef, err)
	}
	return nil
}
