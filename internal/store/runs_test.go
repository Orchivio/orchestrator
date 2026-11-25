package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/router"
)

func newProjectRun(t *testing.T, s *Store, c router.Conversation, project uuid.UUID, ticket, state string, created time.Time) router.Run {
	t.Helper()
	ctx := context.Background()
	r := router.Run{ID: uuid.New(), Tenant: c.Tenant, ConversationID: c.ID, Project: c.Project, ProjectID: project, TicketRef: ticket,
		Kind: "task", State: state, CreatedAt: created, UpdatedAt: created}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestListProjectRunsKeysetPagesAreStable(t *testing.T) {
	s, project, ctx := newStore(t), uuid.New(), context.Background()
	c := newConversation(t, s)
	for range 5 {
		newProjectRun(t, s, c, project, "RES-1", "running", base)
	}
	var seen []uuid.UUID
	var after *admin.RunCursor
	for page := 0; page < 3; page++ {
		got, err := s.ListProjectRuns(ctx, c.Tenant, project, admin.RunFilter{}, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range got {
			seen = append(seen, r.ID)
		}
		after = &admin.RunCursor{CreatedAt: got[len(got)-1].CreatedAt, ID: got[len(got)-1].ID}
	}
	unique := map[uuid.UUID]bool{}
	for _, id := range seen {
		unique[id] = true
	}
	if len(seen) != 5 || len(unique) != 5 {
		t.Errorf("seen %d ids, %d unique, want 5/5", len(seen), len(unique))
	}
}

func TestListProjectRunsFiltersAndScopes(t *testing.T) {
	s, project, ctx := newStore(t), uuid.New(), context.Background()
	c := newConversation(t, s)
	other := newConversation(t, s)
	want := newProjectRun(t, s, c, project, "RES-1", "failed", base)
	newProjectRun(t, s, c, project, "RES-2", "failed", base.Add(time.Minute))
	newProjectRun(t, s, c, project, "RES-1", "running", base.Add(2*time.Minute))
	newProjectRun(t, s, c, uuid.New(), "RES-1", "failed", base) // other project
	newProjectRun(t, s, c, uuid.Nil, "RES-1", "failed", base)   // no project
	newProjectRun(t, s, other, project, "RES-1", "failed", base)

	got, err := s.ListProjectRuns(ctx, c.Tenant, project, admin.RunFilter{Ticket: "RES-1", State: "failed"}, nil, 10)
	if err != nil || len(got) != 1 || got[0].ID != want.ID {
		t.Fatalf("got %+v, err %v, want only %s", got, err, want.ID)
	}
	if all, _ := s.ListProjectRuns(ctx, c.Tenant, project, admin.RunFilter{}, nil, 10); len(all) != 3 || all[0].State != "running" {
		t.Errorf("all = %+v, want 3 newest first", all)
	}
}

func TestGetProjectRunScopesAndReturnsEvents(t *testing.T) {
	s, project, ctx := newStore(t), uuid.New(), context.Background()
	c := newConversation(t, s)
	r := newProjectRun(t, s, c, project, "RES-1", "running", base)
	orphan := newProjectRun(t, s, c, uuid.Nil, "RES-1", "running", base)
	if err := s.AppendEvent(ctx, c.Tenant, r.ID, "message", json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}

	got, events, err := s.GetProjectRun(ctx, c.Tenant, project, r.ID)
	if err != nil || got.ID != r.ID || len(events) != 1 || events[0].Type != "message" {
		t.Fatalf("got %+v %+v err %v", got, events, err)
	}
	for name, call := range map[string]func() error{
		"other project": func() error { _, _, err := s.GetProjectRun(ctx, c.Tenant, uuid.New(), r.ID); return err },
		"other tenant":  func() error { _, _, err := s.GetProjectRun(ctx, "x", project, r.ID); return err },
		"no project id": func() error { _, _, err := s.GetProjectRun(ctx, c.Tenant, project, orphan.ID); return err },
		"unknown run":   func() error { _, _, err := s.GetProjectRun(ctx, c.Tenant, project, uuid.New()); return err },
	} {
		if err := call(); !errors.Is(err, admin.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", name, err)
		}
	}
}
