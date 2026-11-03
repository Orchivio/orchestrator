package reporter

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/router"
)

const cfgYAML = `tenant: acme
tracker: { name: linear, address: x }
codeHost: { name: github, address: y }
profiles:
  default: { runtime: claude-code, model: m, maxDuration: 20m }
projects:
  - name: demo
    trackerKey: ORC
    profile: default
    repos: [ { slug: a/b, baseBranch: main, allowedPaths: [""] } ]
    milestones: [result]
`

type fakeTracker struct{ refs, bodies []string }

func (f *fakeTracker) PostComment(_ context.Context, ref, body string) error {
	f.refs, f.bodies = append(f.refs, ref), append(f.bodies, body)
	return nil
}

func newReporter(t *testing.T) (*Reporter, *fakeTracker) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	sync, err := config.NewSync(dir, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTracker{}
	return New(func(string) TrackerClient { return ft }, sync, "https://orc.example/"), ft
}

func TestReporterSkipsDisabledMilestone(t *testing.T) {
	r, ft := newReporter(t)
	run := router.Run{ID: uuid.New(), Tenant: "acme", Project: "ORC", TicketRef: "ORC-1"}
	if err := r.Milestone(context.Background(), run, config.MilestoneStarted, "go"); err != nil || len(ft.bodies) != 0 {
		t.Fatalf("err=%v bodies=%v", err, ft.bodies)
	}
}

func TestReporterRedactsAndLinksTimeline(t *testing.T) {
	r, ft := newReporter(t)
	run := router.Run{ID: uuid.New(), Tenant: "acme", Project: "ORC", TicketRef: "ORC-1"}
	secret := "ghp_" + strings.Repeat("b", 36)
	if err := r.Milestone(context.Background(), run, config.MilestoneResult, "key "+secret); err != nil {
		t.Fatal(err)
	}
	want := "key ****\n\n[Timeline](https://orc.example/runs/" + run.ID.String() + ")"
	if len(ft.bodies) != 1 || ft.bodies[0] != want || ft.refs[0] != "ORC-1" {
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
