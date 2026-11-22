package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/router"
)

func newTenant(t *testing.T, s *Store) admin.Tenant {
	t.Helper()
	tn, err := s.CreateTenant(context.Background(), admin.Tenant{Slug: "t-" + uuid.NewString(),
		DisplayName: "T", TrackerAdapter: "linear", CodeHostAdapter: "github"})
	if err != nil {
		t.Fatal(err)
	}
	return tn
}

func newProfile(t *testing.T, s *Store, tenant string) admin.Profile {
	t.Helper()
	p, err := s.CreateProfile(context.Background(), admin.Profile{ID: uuid.New(), TenantSlug: tenant, Name: "default",
		Runtime: "claude-code", Model: "m", Instructions: "i", MaxDuration: 20 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func projectIn(tenant string, profile uuid.UUID, key string) admin.Project {
	return admin.Project{ID: uuid.New(), TenantSlug: tenant, Name: "demo", TrackerKey: key, ProfileID: profile,
		Repos: []config.Repo{{Slug: "a/b", BaseBranch: "main", AllowedPaths: []string{""}}}, MaxDiffLines: 500}
}

func createProject(t *testing.T, s *Store) admin.Project {
	t.Helper()
	tn := newTenant(t, s)
	p, err := s.CreateProject(context.Background(), projectIn(tn.Slug, newProfile(t, s, tn.Slug).ID, "K-"+uuid.NewString()), "linear")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func withName(p admin.Project, name string) admin.Project {
	p.Name = name
	return p
}

func TestTenantSlugNeverReused(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tn := newTenant(t, s)
	if err := s.DeleteTenant(ctx, tn.Slug, 0); err != nil {
		t.Fatal(err)
	}
	_, err := s.CreateTenant(ctx, tn)
	if !errors.Is(err, admin.ErrTaken) {
		t.Fatalf("err = %v", err)
	}
}

func TestTrackerKeyUniquePerAdapter(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, b := newTenant(t, s), newTenant(t, s)
	key := "RES-" + uuid.NewString()
	first, err := s.CreateProject(ctx, projectIn(a.Slug, newProfile(t, s, a.Slug).ID, key), "linear")
	if err != nil {
		t.Fatal(err)
	}
	other := projectIn(b.Slug, newProfile(t, s, b.Slug).ID, key)
	if _, err = s.CreateProject(ctx, other, "linear"); !errors.Is(err, admin.ErrTaken) {
		t.Fatalf("err = %v", err)
	}
	if err = s.DeleteProject(ctx, a.Slug, first.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateProject(ctx, other, "linear"); !errors.Is(err, admin.ErrTaken) {
		t.Fatalf("after delete err = %v", err)
	}
}

func TestUpdateProjectWithStaleVersionFails(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	p := createProject(t, s)
	updated, err := s.UpdateProject(ctx, withName(p, "x"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != 2 {
		t.Fatalf("version = %d", updated.Version)
	}
	_, err = s.UpdateProject(ctx, withName(p, "y"), 1)
	if !errors.Is(err, admin.ErrVersionMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestUpdateMissingProjectIsNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.UpdateProject(context.Background(), projectIn("nope", uuid.New(), "X"), 0)
	if !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestDeleteProjectBlockedByUnfinishedRun(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	p := createProject(t, s)
	c := router.Conversation{ID: uuid.New(), Tenant: p.TenantSlug, Project: p.TrackerKey, TicketRef: "K-1", CreatedAt: base}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	run := newRun(t, s, c, "running", base)
	if err := s.DeleteProject(ctx, p.TenantSlug, p.ID, 0); !errors.Is(err, admin.ErrInUse) {
		t.Fatalf("err = %v", err)
	}
	run.State = "succeeded"
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, p.TenantSlug, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(ctx, p.TenantSlug, p.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("get err = %v", err)
	}
}

func TestDeleteProfileInUse(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	p := createProject(t, s)
	if err := s.DeleteProfile(ctx, p.TenantSlug, p.ProfileID, 0); !errors.Is(err, admin.ErrInUse) {
		t.Fatalf("err = %v", err)
	}
}

func TestProjectSpecRoundTrip(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tn := newTenant(t, s)
	in := projectIn(tn.Slug, newProfile(t, s, tn.Slug).ID, "K-"+uuid.NewString())
	in.Repos = []config.Repo{{Slug: "a/b", BaseBranch: "main", AllowedPaths: []string{""}, Users: []string{"u@x.de"}}}
	in.Classifier = config.ClassifierRules{TaskLabels: []string{"t"}, QuestionLabels: []string{"q"},
		RepoLabels: map[string]string{"repo:b": "a/b"}, DefaultRepo: "a/b"}
	in.Roles = config.Roles{Questions: []string{"q@x.de"}, Tasks: []string{"t@x.de"}}
	in.Milestones = []string{"started", "result"}
	if _, err := s.CreateProject(ctx, in, "linear"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetProject(ctx, tn.Slug, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Repos, in.Repos) || !reflect.DeepEqual(got.Classifier, in.Classifier) ||
		!reflect.DeepEqual(got.Roles, in.Roles) || !reflect.DeepEqual(got.Milestones, in.Milestones) || got.MaxDiffLines != 500 {
		t.Fatalf("got %+v, want %+v", got, in)
	}
}

func TestRunStoresProjectID(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	r := newRun(t, s, c, "queued", base)
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.ProjectID != uuid.Nil {
		t.Fatalf("nil project id: %v %v", got, err)
	}
	r.ID, r.ProjectID = uuid.New(), uuid.New()
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetRun(ctx, r.ID)
	if err != nil || got.ProjectID != r.ProjectID {
		t.Fatalf("got %v, %v", got, err)
	}
}

// Pins the stored jsonb format: renaming a Go field must not silently drop data.
func TestProjectSpecStoredKeys(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	tn := newTenant(t, s)
	in := projectIn(tn.Slug, newProfile(t, s, tn.Slug).ID, "K-"+uuid.NewString())
	in.Repos = []config.Repo{{Slug: "a/b", BaseBranch: "main", AllowedPaths: []string{""}, Users: []string{"u@x.de"}}}
	in.Classifier = config.ClassifierRules{TaskLabels: []string{"t"}, QuestionLabels: []string{"q"},
		RepoLabels: map[string]string{"repo:b": "a/b"}, DefaultRepo: "a/b"}
	in.Roles = config.Roles{Questions: []string{"q@x.de"}, Tasks: []string{"t@x.de"}}
	in.Milestones = []string{"started"}
	if _, err := s.CreateProject(ctx, in, "linear"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx, "SELECT spec FROM projects WHERE id = $1", in.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	const want = `{"repos":[{"slug":"a/b","baseBranch":"main","allowedPaths":[""],"users":["u@x.de"]}],
		"classifier":{"taskLabels":["t"],"questionLabels":["q"],"repoLabels":{"repo:b":"a/b"},"defaultRepo":"a/b"},
		"roles":{"questions":["q@x.de"],"tasks":["t@x.de"]},"maxDiffLines":500,"milestones":["started"]}`
	var got, wantDoc map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantDoc); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantDoc) {
		t.Fatalf("stored spec = %s", raw)
	}
}

func TestTokenLifecycle(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	before, err := s.CountTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hash := []byte(uuid.NewString())
	tok := admin.Token{ID: uuid.New(), Name: "ci", Prefix: "orv_abcdefgh", Scope: admin.ScopeInstance, Role: admin.RoleAdmin,
		CreatedAt: base, ExpiresAt: base.Add(time.Hour)}
	if _, err := s.CreateToken(ctx, tok, hash); err != nil {
		t.Fatal(err)
	}
	got, err := s.TokenByHash(ctx, hash)
	if err != nil || got.ID != tok.ID || got.RevokedAt != nil || !got.ExpiresAt.Equal(tok.ExpiresAt) {
		t.Fatalf("got %+v err %v", got, err)
	}
	if err := s.RevokeToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.TokenByHash(ctx, hash); got.RevokedAt == nil {
		t.Fatal("not revoked")
	}
	if n, _ := s.CountTokens(ctx); n != before+1 {
		t.Fatalf("revoked token must still count: %d vs %d", n, before)
	}
	list, _ := s.ListTokens(ctx)
	if len(list) != before+1 || list[0].ID != tok.ID {
		t.Fatalf("newest first expected, got %d rows", len(list))
	}
}

func TestTokenNotFound(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	if _, err := s.TokenByHash(ctx, []byte("nope")); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("by hash: %v", err)
	}
	if err := s.RevokeToken(ctx, uuid.New()); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("revoke: %v", err)
	}
}
