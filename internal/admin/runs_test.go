package admin_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/router"
)

var runsBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// runsFixture has tenant acme with a project holding five runs created at the same instant.
func runsFixture(t *testing.T) (svc *admin.Service, projectID uuid.UUID, runs []router.Run) {
	t.Helper()
	st := newMemStore()
	projectID = uuid.New()
	st.tenants["acme"] = admin.Tenant{Slug: "acme"}
	st.projects[projectID] = admin.Project{ID: projectID, TenantSlug: "acme"}
	for range 5 {
		r := router.Run{ID: uuid.New(), Tenant: "acme", ProjectID: projectID, TicketRef: "RES-1", State: "running", CreatedAt: runsBase}
		st.addRun(r)
		runs = append(runs, r)
	}
	return admin.NewService(st, nil, nil, nil, "", time.Now, uuid.New, nil), projectID, runs
}

func TestListRunsPagesCoverEveryRunOnce(t *testing.T) {
	svc, projectID, runs := runsFixture(t)
	seen, sizes, before := map[uuid.UUID]bool{}, []int{}, ""
	for {
		page, err := svc.ListRuns(ctx, "acme", projectID, admin.RunFilter{Limit: 2, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(page.Runs))
		for _, r := range page.Runs {
			if seen[r.ID] {
				t.Fatalf("run %s listed twice", r.ID)
			}
			seen[r.ID] = true
		}
		if before = page.Next; before == "" {
			break
		}
	}
	if len(seen) != len(runs) || len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Errorf("seen %d runs in pages %v, want 5 in [2 2 1]", len(seen), sizes)
	}
}

func TestListRunsRejectsBadLimitAndCursor(t *testing.T) {
	svc, projectID, _ := runsFixture(t)
	cases := []struct {
		name  string
		f     admin.RunFilter
		field string
	}{
		{"limit below 1", admin.RunFilter{Limit: -1}, "limit"},
		{"limit above 200", admin.RunFilter{Limit: 201}, "limit"},
		{"garbage cursor", admin.RunFilter{Before: "garbage"}, "before"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.ListRuns(ctx, "acme", projectID, c.f)
			var ve *admin.ValidationError
			if !errors.As(err, &ve) || !hasField(ve, c.field) {
				t.Fatalf("err = %v, want validation error on %s", err, c.field)
			}
		})
	}
}

func TestRunsOfForeignProjectOrTenantAreNotFound(t *testing.T) {
	svc, projectID, runs := runsFixture(t)
	if _, err := svc.ListRuns(ctx, "acme", uuid.New(), admin.RunFilter{}); !errors.Is(err, admin.ErrNotFound) {
		t.Errorf("unknown project: err = %v", err)
	}
	if _, _, err := svc.GetRun(ctx, "acme", uuid.New(), runs[0].ID); !errors.Is(err, admin.ErrNotFound) {
		t.Errorf("run via unknown project: err = %v", err)
	}
	if _, _, err := svc.GetRun(ctx, "acme", projectID, uuid.New()); !errors.Is(err, admin.ErrNotFound) {
		t.Errorf("unknown run: err = %v", err)
	}
}
