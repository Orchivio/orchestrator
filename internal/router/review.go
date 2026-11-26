package router

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/classifier"
	"github.com/orchivio/orchestrator/internal/config"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

const (
	// NoticeReviewNoPermission answers a review comment whose author lacks the tasks role.
	NoticeReviewNoPermission = "Ein Review-Kommentar wurde nicht umgesetzt: keine Berechtigung für Aufgaben."

	reviewBodyFormat     = "Review-Kommentar (%s):\n%s"
	codeHostSourcePrefix = "codehost:"
)

// HandleReview turns a review comment on the pull request of a task run into
// a follow-up run of that run's conversation, on the same branch.
func (r *Router) HandleReview(ctx context.Context, tenant string, ev *pluginv1.CodeHostEvent) error {
	if ev.GetKind() != pluginv1.CodeHostEvent_REVIEW_COMMENTED || ev.GetBranch() == "" {
		return nil
	}
	if ev.GetActor().GetId() == "" && ev.GetActor().GetUsername() == "" {
		return nil // bots and unknown authors; the adapter already drops its own comments
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg := r.cfg.Current()
	if _, ok := cfg.Tenants[tenant]; !ok {
		return nil
	}
	system, err := r.codeHost.AdapterName(ctx, tenant)
	if err != nil {
		return fmt.Errorf("code host adapter name of tenant %s: %w", tenant, err)
	}
	fresh, err := r.store.MarkInbound(ctx, tenant, codeHostSourcePrefix+system, ev.GetExternalId())
	if err != nil {
		return fmt.Errorf("mark inbound review %s: %w", ev.GetExternalId(), err)
	}
	if !fresh {
		return nil
	}
	run, err := r.store.FindTaskRunByBranch(ctx, tenant, ev.GetRepo(), ev.GetBranch())
	if err != nil {
		return fmt.Errorf("find run of branch %s in %s: %w", ev.GetBranch(), ev.GetRepo(), err)
	}
	if run == nil {
		slog.Debug("review comment without task run dropped", "tenant", tenant, "repo", ev.GetRepo(), "branch", ev.GetBranch())
		return nil
	}
	project, ok := projectByID(cfg.Tenants[tenant], run.ProjectID)
	if !ok {
		slog.Debug("review comment on run without configured project dropped", "tenant", tenant, "run", run.ID)
		return nil
	}
	if project.EffectiveReviewTrigger() == config.ReviewTriggerMention && !ev.GetMentionsBot() {
		return nil
	}
	actors := classifier.Identities(system, ev.GetActor())
	if !classifier.HasTasksRole(project, actors) {
		return r.reporter.Notice(ctx, tenant, run.TicketRef, NoticeReviewNoPermission)
	}
	in := Input{ID: r.newID(), Tenant: tenant, ConversationID: run.ConversationID,
		ActorEmail: classifier.FirstMatch(project, actors), Actors: actors, Trigger: classifier.TriggerFollowup,
		Body: fmt.Sprintf(reviewBodyFormat, ev.GetPrUrl(), ev.GetBody()), CreatedAt: r.now()}
	if err := r.store.AddInput(ctx, in); err != nil {
		return fmt.Errorf("add review input %s: %w", run.TicketRef, err)
	}
	return r.startFollowUp(ctx, run.ConversationID)
}

// projectByID finds a project of the tenant by id; runs from before managed
// projects (uuid.Nil) match none.
func projectByID(t config.Tenant, id uuid.UUID) (config.Project, bool) {
	for _, p := range t.Projects {
		if id != uuid.Nil && p.ID == id {
			return p, true
		}
	}
	return config.Project{}, false
}
