package store

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/orchivio/orchestrator/internal/admin"
)

func connIn(tenant, kind, adapter, external string) admin.Connection {
	return admin.Connection{ID: uuid.New(), TenantSlug: tenant, Kind: kind, Adapter: adapter,
		ExternalID: external, Label: "l", Status: admin.StatusActive}
}

var cred = admin.SealedCredential{KeyID: "k1", Data: []byte("sealed")}

func TestSaveConnectionIsExclusiveAcrossTenants(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, b, ext := newTenant(t, s), newTenant(t, s), uuid.NewString()
	if _, err := s.SaveConnection(ctx, connIn(a.Slug, admin.KindCodeHost, "github", ext), cred); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveConnection(ctx, connIn(b.Slug, admin.KindCodeHost, "github", ext), cred)
	if !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestSaveConnectionReconnectUpdatesSameRow(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, ext := newTenant(t, s), uuid.NewString()
	due1, due2 := base.Add(time.Hour), base.Add(2*time.Hour)
	in := connIn(a.Slug, admin.KindCodeHost, "github", ext)
	in.RefreshAfter = &due1
	first, err := s.SaveConnection(ctx, in, cred)
	if err != nil {
		t.Fatal(err)
	}
	if first.RefreshAfter == nil || !first.RefreshAfter.Equal(due1) {
		t.Fatalf("insert refresh_after = %v", first.RefreshAfter)
	}
	if err = s.UpdateConnectionCredential(ctx, first.ID, cred, nil, admin.StatusNeedsReauth); err != nil {
		t.Fatal(err)
	}
	again := connIn(a.Slug, admin.KindCodeHost, "github", ext)
	again.Label = "new"
	again.RefreshAfter = &due2
	second, err := s.SaveConnection(ctx, again, admin.SealedCredential{KeyID: "k2", Data: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ConnectionCredential(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Status != admin.StatusActive || second.Label != "new" || second.RefreshAfter == nil || !second.RefreshAfter.Equal(due2) || got.KeyID != "k2" || string(got.Data) != "new" {
		t.Fatalf("second = %+v, cred = %+v", second, got)
	}
}

func TestOnlyOneLiveTrackerConnectionPerTenant(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a := newTenant(t, s)
	first, err := s.SaveConnection(ctx, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveConnection(ctx, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred)
	if !errors.Is(err, admin.ErrTaken) {
		t.Fatalf("err = %v", err)
	}
	if err = s.DeleteConnection(ctx, a.Slug, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SaveConnection(ctx, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred); err != nil {
		t.Fatalf("after delete: %v", err)
	}
	if list, err := s.ListConnections(ctx, a.Slug); err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if _, err = s.GetConnection(ctx, a.Slug, first.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("deleted get err = %v", err)
	}
}

func TestOAuthStateSingleUseAndExpiry(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a := newTenant(t, s)
	hash, expired := []byte(uuid.NewString()), []byte(uuid.NewString())
	if err := s.CreateOAuthState(ctx, hash, a.Slug, "linear", admin.KindTracker, base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOAuthState(ctx, expired, a.Slug, "linear", admin.KindTracker, base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	tenant, adapter, kind, err := s.ConsumeOAuthState(ctx, hash, base)
	if err != nil || tenant != a.Slug || adapter != "linear" || kind != admin.KindTracker {
		t.Fatalf("got %q %q %q, %v", tenant, adapter, kind, err)
	}
	if _, _, _, err = s.ConsumeOAuthState(ctx, hash, base); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("second consume err = %v", err)
	}
	if _, _, _, err = s.ConsumeOAuthState(ctx, expired, base); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("expired consume err = %v", err)
	}
}

func TestClaimDueConnectionsLeases(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a := newTenant(t, s)
	due, err := s.SaveConnection(ctx, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred)
	if err != nil {
		t.Fatal(err)
	}
	past := base.Add(-time.Hour)
	if err = s.UpdateConnectionCredential(ctx, due.ID, cred, &past, admin.StatusActive); err != nil {
		t.Fatal(err)
	}
	lease := base.Add(10 * time.Minute)
	conns, creds, err := s.ClaimDueConnections(ctx, base, lease)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for i, c := range conns {
		if c.ID == due.ID {
			found = true
			if creds[i].KeyID != cred.KeyID {
				t.Fatalf("cred = %+v", creds[i])
			}
		}
	}
	if !found {
		t.Fatal("due connection not claimed")
	}
	again, _, err := s.ClaimDueConnections(ctx, base, lease)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range again {
		if c.ID == due.ID {
			t.Fatal("claimed twice before lease expired")
		}
	}
	later, _, err := s.ClaimDueConnections(ctx, lease, lease.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var reclaimed bool
	for _, c := range later {
		reclaimed = reclaimed || c.ID == due.ID
	}
	if !reclaimed {
		t.Fatal("not reclaimed after lease expired")
	}
}

func TestListAllActiveConnectionsSkipsNeedsReauthAndDeleted(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a := newTenant(t, s)
	ok, _ := s.SaveConnection(ctx, connIn(a.Slug, admin.KindCodeHost, "github", uuid.NewString()), cred)
	bad, _ := s.SaveConnection(ctx, connIn(a.Slug, admin.KindCodeHost, "github", uuid.NewString()), cred)
	gone, _ := s.SaveConnection(ctx, connIn(a.Slug, admin.KindCodeHost, "github", uuid.NewString()), cred)
	if err := s.UpdateConnectionCredential(ctx, bad.ID, cred, nil, admin.StatusNeedsReauth); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteConnection(ctx, a.Slug, gone.ID); err != nil {
		t.Fatal(err)
	}
	conns, creds, err := s.ListAllActiveConnections(ctx)
	if err != nil || len(conns) != len(creds) {
		t.Fatalf("%v %d %d", err, len(conns), len(creds))
	}
	ids := map[uuid.UUID]bool{}
	for _, c := range conns {
		ids[c.ID] = true
	}
	if !ids[ok.ID] || ids[bad.ID] || ids[gone.ID] {
		t.Fatalf("ids = %v", ids)
	}
}

func TestTrackerKeyUniquePerTenantOnly(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, b := newTenant(t, s), newTenant(t, s)
	pa, pb := newProfile(t, s, a.Slug), newProfile(t, s, b.Slug)
	if _, err := s.CreateProject(ctx, projectIn(a.Slug, pa.ID, "RES"), "linear"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProject(ctx, projectIn(b.Slug, pb.ID, "RES"), "linear"); err != nil {
		t.Fatalf("other tenant: %v", err)
	}
	if _, err := s.CreateProject(ctx, projectIn(a.Slug, pa.ID, "RES"), "linear"); !errors.Is(err, admin.ErrTaken) {
		t.Fatalf("same tenant err = %v", err)
	}
}

func TestInboundDedupIsPerTenant(t *testing.T) {
	s, ctx, id := newStore(t), context.Background(), uuid.NewString()
	for _, tenant := range []string{"a-" + id, "b-" + id} {
		fresh, err := s.MarkInbound(ctx, tenant, "linear", id)
		if err != nil || !fresh {
			t.Fatalf("%s: %v, %v", tenant, fresh, err)
		}
	}
}

func TestMigrationBackfillsInboundTenantWhenSingleTenant(t *testing.T) {
	ctx := context.Background()
	admin0, err := pgx.Connect(ctx, testURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin0.Close(ctx)
	name := "mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin0.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(testURL)
	u.Path = "/" + name
	m := newMigrator(t, u.String())
	if err = m.Migrate(3); err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `INSERT INTO tenants (slug, display_name, tracker_adapter, code_host_adapter, created_at, updated_at)
		VALUES ('acme', 'A', 'linear', 'github', now(), now());
		INSERT INTO inbound_events (source, external_id, received_at) VALUES ('linear', 'e1', now())`)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Up(); err != nil {
		t.Fatal(err)
	}
	var tenant string
	if err = conn.QueryRow(ctx, "SELECT tenant_id FROM inbound_events WHERE external_id = 'e1'").Scan(&tenant); err != nil || tenant != "acme" {
		t.Fatalf("tenant = %q, %v", tenant, err)
	}
	if _, err = conn.Exec(ctx, "INSERT INTO inbound_events (source, external_id, received_at) VALUES ('x', 'y', now())"); err == nil {
		t.Fatal("insert without tenant_id must fail after migration")
	}
}

func newMigrator(t *testing.T, databaseURL string) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	murl, err := migrationURL(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, murl)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Close() })
	return m
}

func TestReplaceTrackerConnectionIsAllOrNothing(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a, b := newTenant(t, s), newTenant(t, s)
	old, err := s.SaveConnection(ctx, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred)
	if err != nil {
		t.Fatal(err)
	}
	foreign := uuid.NewString()
	if _, err := s.SaveConnection(ctx, connIn(b.Slug, admin.KindTracker, "linear", foreign), cred); err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplaceTrackerConnection(ctx, a.Slug, connIn(a.Slug, admin.KindTracker, "linear", foreign), cred)
	if !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if got, err := s.GetConnection(ctx, a.Slug, old.ID); err != nil || got.Status != admin.StatusActive {
		t.Fatalf("old connection after failed switch: %+v %v", got, err)
	}
	next, err := s.ReplaceTrackerConnection(ctx, a.Slug, connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString()), cred)
	if err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListConnections(ctx, a.Slug); err != nil || len(list) != 1 || list[0].ID != next.ID {
		t.Fatalf("list = %+v %v", list, err)
	}
	again, err := s.ReplaceTrackerConnection(ctx, a.Slug, connIn(a.Slug, admin.KindTracker, "linear", next.ExternalID), cred)
	if err != nil || again.ID != next.ID {
		t.Fatalf("reconnect same workspace: %+v %v", again, err)
	}
}

func TestUpdateConnectionCredentialIfSkipsRowChangedSinceClaim(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	a := newTenant(t, s)
	c := connIn(a.Slug, admin.KindTracker, "linear", uuid.NewString())
	saved, err := s.SaveConnection(ctx, c, cred)
	if err != nil {
		t.Fatal(err)
	}
	past := base.Add(-time.Hour)
	if err = s.UpdateConnectionCredential(ctx, saved.ID, cred, &past, admin.StatusActive); err != nil {
		t.Fatal(err)
	}
	before, err := s.GetConnection(ctx, a.Slug, saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := claimOne(t, s, saved.ID)
	if !claimed.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("claim moved updated_at from %v to %v", before.UpdatedAt, claimed.UpdatedAt)
	}
	reconnected := admin.SealedCredential{KeyID: "k1", Data: []byte("reconnected")}
	if _, err = s.SaveConnection(ctx, c, reconnected); err != nil {
		t.Fatal(err)
	}

	ok, err := s.UpdateConnectionCredentialIf(ctx, saved.ID, claimed.UpdatedAt, admin.SealedCredential{KeyID: "k1", Data: []byte("stale")}, nil, admin.StatusNeedsReauth)

	got, _ := s.ConnectionCredential(ctx, saved.ID)
	now, _ := s.GetConnection(ctx, a.Slug, saved.ID)
	if err != nil || ok || string(got.Data) != "reconnected" || now.Status != admin.StatusActive {
		t.Fatalf("ok=%v err=%v cred=%q status=%s", ok, err, got.Data, now.Status)
	}
	ok, err = s.UpdateConnectionCredentialIf(ctx, saved.ID, now.UpdatedAt, admin.SealedCredential{KeyID: "k1", Data: []byte("fresh")}, nil, admin.StatusActive)
	got, _ = s.ConnectionCredential(ctx, saved.ID)
	if err != nil || !ok || string(got.Data) != "fresh" {
		t.Fatalf("current version: ok=%v err=%v cred=%q", ok, err, got.Data)
	}
}

// claimOne claims due connections and returns the one with id.
func claimOne(t *testing.T, s *Store, id uuid.UUID) admin.Connection {
	t.Helper()
	conns, _, err := s.ClaimDueConnections(context.Background(), base, base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range conns {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("connection %s not claimed", id)
	return admin.Connection{}
}
