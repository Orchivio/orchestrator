package router

import (
	"context"
	"testing"

	"github.com/orchivio/orchestrator/internal/classifier"
	"github.com/orchivio/orchestrator/internal/config"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

const (
	reviewRepo = "soemii/playground"
	prURL      = "https://host.example/mr/1"
)

// finishedTask creates a succeeded task run of ORC-12 for tenant via the normal path.
func finishedTask(t *testing.T, f *fixture, tenant string) *Run {
	t.Helper()
	if err := f.r.Handle(context.Background(), tenant, assigned("a-"+tenant, actor)); err != nil {
		t.Fatal(err)
	}
	run := f.s.runs[len(f.s.runs)-1]
	run.State = StateSucceeded
	return run
}

func review(id, branch string, mention bool, a *pluginv1.Actor) *pluginv1.CodeHostEvent {
	return &pluginv1.CodeHostEvent{ExternalId: id, Kind: pluginv1.CodeHostEvent_REVIEW_COMMENTED, Repo: reviewRepo,
		Branch: branch, PrUrl: prURL, Actor: a, Body: "bitte umbenennen", MentionsBot: mention}
}

func tasker() *pluginv1.Actor { return &pluginv1.Actor{Id: "7", Username: "dev", Email: actor} }

func handleReview(t *testing.T, f *fixture, tenant string, ev *pluginv1.CodeHostEvent) {
	t.Helper()
	if err := f.r.HandleReview(context.Background(), tenant, ev); err != nil {
		t.Fatalf("HandleReview: %v", err)
	}
}

func TestReviewCommentStartsFollowUp(t *testing.T) {
	f := newFixture(t, reviewRepo)
	first := finishedTask(t, f, "acme")

	handleReview(t, f, "acme", review("r1", first.Branch, true, tasker()))

	if len(f.s.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(f.s.runs))
	}
	next := f.s.runs[1]
	if next.ConversationID != first.ConversationID || next.Branch != first.Branch || next.Kind != classifier.KindTask ||
		next.State != StateQueued {
		t.Fatalf("follow-up run = %+v, first = %+v", next, first)
	}
	in := f.s.inputs[len(f.s.inputs)-1]
	if want := "Review-Kommentar (" + prURL + "):\nbitte umbenennen"; in.Body != want || in.Trigger != classifier.TriggerFollowup {
		t.Fatalf("input = %q %s, want %q followup", in.Body, in.Trigger, want)
	}
	if in.RunID == nil || *in.RunID != next.ID {
		t.Fatal("review input not claimed by the follow-up run")
	}
}

func TestReviewWithoutMentionIgnoredByDefault(t *testing.T) {
	f := newFixture(t, reviewRepo)
	first := finishedTask(t, f, "acme")
	inputs := len(f.s.inputs)

	handleReview(t, f, "acme", review("r1", first.Branch, false, tasker()))

	if len(f.s.runs) != 1 || len(f.s.inputs) != inputs || len(f.rep.notices) != 0 {
		t.Fatalf("runs=%d inputs=%d notices=%v", len(f.s.runs), len(f.s.inputs), f.rep.notices)
	}
}

func TestReviewAnyTriggerWithoutMention(t *testing.T) {
	f := newFixtureWith(t, reviewRepo, func(p *config.Project) { p.ReviewTrigger = config.ReviewTriggerAny })
	first := finishedTask(t, f, "acme")

	handleReview(t, f, "acme", review("r1", first.Branch, false, tasker()))

	if len(f.s.runs) != 2 || f.s.runs[1].Branch != first.Branch {
		t.Fatalf("runs = %d, want a follow-up on %s", len(f.s.runs), first.Branch)
	}
}

func TestReviewWithoutRolePostsNotice(t *testing.T) {
	f := newFixture(t, reviewRepo)
	first := finishedTask(t, f, "acme")
	inputs := len(f.s.inputs)

	handleReview(t, f, "acme", review("r1", first.Branch, true, &pluginv1.Actor{Id: "9", Username: "stranger"}))

	if len(f.s.runs) != 1 || len(f.s.inputs) != inputs {
		t.Fatalf("runs=%d inputs=%d, want no run and no input", len(f.s.runs), len(f.s.inputs))
	}
	if len(f.rep.notices) != 1 || f.rep.notices[0] != NoticeReviewNoPermission {
		t.Fatalf("notices = %v", f.rep.notices)
	}
}

func TestReviewUnknownBranchDropped(t *testing.T) {
	f := newFixture(t, reviewRepo)
	finishedTask(t, f, "acme")
	inputs := len(f.s.inputs)

	handleReview(t, f, "acme", review("r1", "feature/elsewhere", true, tasker()))

	if len(f.s.runs) != 1 || len(f.s.inputs) != inputs || len(f.rep.notices) != 0 {
		t.Fatalf("runs=%d inputs=%d notices=%v", len(f.s.runs), len(f.s.inputs), f.rep.notices)
	}
}

func TestReviewEventFromBotIsIgnored(t *testing.T) {
	f := newFixture(t, reviewRepo)
	first := finishedTask(t, f, "acme")
	inputs := len(f.s.inputs)

	// The e-mail alone holds the tasks role; without id and username the event is still dropped.
	handleReview(t, f, "acme", review("r1", first.Branch, true, &pluginv1.Actor{Email: actor}))

	if len(f.s.runs) != 1 || len(f.s.inputs) != inputs || len(f.rep.notices) != 0 {
		t.Fatalf("runs=%d inputs=%d notices=%v", len(f.s.runs), len(f.s.inputs), f.rep.notices)
	}
}

func TestReviewEventMatchesOnlyOwnTenant(t *testing.T) {
	cfg := testConfig(reviewRepo, nil)
	beta := cfg.Tenants["acme"]
	beta.Name = "beta"
	cfg.Tenants["beta"] = beta
	f := newFixtureFor(t, cfg)
	a := finishedTask(t, f, "acme")
	b := finishedTask(t, f, "beta")
	b.Branch = a.Branch

	handleReview(t, f, "beta", review("r1", a.Branch, true, tasker()))

	if len(f.s.runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(f.s.runs))
	}
	if next := f.s.runs[2]; next.Tenant != "beta" || next.ConversationID != b.ConversationID {
		t.Fatalf("follow-up tenant=%s conversation=%s, want beta %s", next.Tenant, next.ConversationID, b.ConversationID)
	}
}

func TestCodeHostEventDedup(t *testing.T) {
	f := newFixture(t, reviewRepo)
	first := finishedTask(t, f, "acme")
	inputs := len(f.s.inputs)

	handleReview(t, f, "acme", review("r1", first.Branch, true, tasker()))
	f.s.runs[1].State = StateSucceeded
	handleReview(t, f, "acme", review("r1", first.Branch, true, tasker()))

	if len(f.s.inputs) != inputs+1 || len(f.s.runs) != 2 {
		t.Fatalf("inputs=%d runs=%d, want %d and 2", len(f.s.inputs), len(f.s.runs), inputs+1)
	}
}
