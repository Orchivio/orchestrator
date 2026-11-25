package reporter

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/router"
)

func testConfig() *config.Config {
	return &config.Config{Tenants: map[string]config.Tenant{"acme": {
		Name:     "acme",
		Tracker:  config.AdapterRef{Name: "linear", Address: "x"},
		CodeHost: config.AdapterRef{Name: "github", Address: "y"},
		Profiles: map[string]config.Profile{"default": {Runtime: "claude-code", Model: "m", MaxDuration: 20 * time.Minute}},
		Projects: []config.Project{{
			Name: "demo", TrackerKey: "ORC", Profile: "default",
			Repos:      []config.Repo{{Slug: "a/b", BaseBranch: "main", AllowedPaths: []string{""}}},
			Milestones: []string{"result"},
		}},
	}}}
}

type fakeTracker struct{ refs, bodies []string }

func (f *fakeTracker) PostComment(_ context.Context, ref, body string) error {
	f.refs, f.bodies = append(f.refs, ref), append(f.bodies, body)
	return nil
}

func newReporter(t *testing.T) (*Reporter, *fakeTracker) {
	t.Helper()
	sync, err := config.NewSync(context.Background(), config.Static{Config: testConfig()}, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTracker{}
	return New(func(string) TrackerClient { return ft }, sync), ft
}

func TestReporterSkipsDisabledMilestone(t *testing.T) {
	r, ft := newReporter(t)
	run := router.Run{ID: uuid.New(), Tenant: "acme", Project: "ORC", TicketRef: "ORC-1"}
	if err := r.Milestone(context.Background(), run, config.MilestoneStarted, "go"); err != nil || len(ft.bodies) != 0 {
		t.Fatalf("err=%v bodies=%v", err, ft.bodies)
	}
}

func TestMilestoneCommentHasRunIDNotLink(t *testing.T) {
	r, ft := newReporter(t)
	run := router.Run{ID: uuid.New(), Tenant: "acme", Project: "ORC", TicketRef: "ORC-1"}
	secret := "ghp_" + strings.Repeat("b", 36)
	if err := r.Milestone(context.Background(), run, config.MilestoneResult, "key "+secret); err != nil {
		t.Fatal(err)
	}
	want := "key ****\n\nRun-ID: `" + run.ID.String() + "`"
	if len(ft.bodies) != 1 || ft.bodies[0] != want || ft.refs[0] != "ORC-1" || strings.Contains(ft.bodies[0], "/runs/") {
		t.Errorf("bodies = %q", ft.bodies)
	}
}

func TestReporterNoticeAlwaysPostsRedactedWithoutLink(t *testing.T) {
	r, ft := newReporter(t)
	secret := "ghp_" + strings.Repeat("c", 36)
	if err := r.Notice(context.Background(), "acme", "ORC-2", "hi "+secret); err != nil {
		t.Fatal(err)
	}
	if len(ft.bodies) != 1 || ft.bodies[0] != "hi ****" {
		t.Errorf("bodies = %q", ft.bodies)
	}
}

func TestReporterNoTrackerClientErrors(t *testing.T) {
	r, _ := newReporter(t)
	r.trackers = func(string) TrackerClient { return nil }
	if err := r.Notice(context.Background(), "acme", "ORC-2", "x"); err == nil {
		t.Fatal("want error")
	}
}
