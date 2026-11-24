package admin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orchivio/orchestrator/internal/admin"
)

// dueConnection connects tenant acme to adapter and makes the connection due for refresh.
func (e *connEnv) dueConnection(t *testing.T, adapter, externalID string) admin.Connection {
	t.Helper()
	c, err := e.connect(t, adapter, externalID)
	if err != nil {
		t.Fatal(err)
	}
	past := e.now.Add(-time.Minute)
	e.store.conns[c.ID].c.RefreshAfter = &past
	return c
}

func (e *connEnv) countChanges() *int {
	n := 0
	e.svc.OnChange(func(context.Context) error { n++; return nil })
	return &n
}

func TestRefreshDueStoresNewCredential(t *testing.T) {
	e := newConnEnv(t)
	c := e.dueConnection(t, "linear", "ws1")
	next := e.now.Add(12 * time.Hour)
	e.conn.refreshed = admin.Completed{Data: []byte("plain-renewed"), RefreshAfter: &next}
	changes := e.countChanges()

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	if len(e.conn.refreshedWith) != 1 || string(e.conn.refreshedWith[0]) != "plain-ws1" {
		t.Fatalf("refresh called with %q", e.conn.refreshedWith)
	}
	mc := e.store.conns[c.ID]
	plain, err := e.box.Open(mc.cred.KeyID, mc.cred.Data)
	if err != nil || string(plain) != "plain-renewed" || mc.c.Status != admin.StatusActive || !mc.c.RefreshAfter.Equal(next) || *changes != 1 {
		t.Fatalf("plain=%q err=%v status=%s refreshAfter=%v changes=%d", plain, err, mc.c.Status, mc.c.RefreshAfter, *changes)
	}
	cfg, err := e.svc.Load(ctx)
	if err != nil || string(cfg.Tenants["acme"].TrackerCredential) != "plain-renewed" {
		t.Fatalf("config does not carry the renewed credential (err %v)", err)
	}
}

func TestRefreshFailureMarksNeedsReauthAndStopsStream(t *testing.T) {
	e := newConnEnv(t)
	c := e.dueConnection(t, "linear", "ws1")
	e.conn.refreshErr = fmt.Errorf("refresh credential at linear: %w", admin.ErrCredentialRejected)
	changes := e.countChanges()

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.GetConnection(ctx, "acme", c.ID)
	if err != nil || got.Status != admin.StatusNeedsReauth || *changes != 1 {
		t.Fatalf("status=%s err=%v changes=%d", got.Status, err, *changes)
	}
	cfg, err := e.svc.Load(ctx)
	if err != nil || cfg.Tenants["acme"].TrackerCredential != nil { // ingest runs no stream without it
		t.Fatalf("needs_reauth credential still in config (err %v)", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, c.ID.String()) || strings.Contains(logs, "plain-ws1") {
		t.Fatalf("logs = %s", logs)
	}
}

func TestRefreshTransientFailureKeepsConnectionForNextAttempt(t *testing.T) {
	e := newConnEnv(t)
	c := e.dueConnection(t, "github", "inst1")
	e.conn.refreshErr = fmt.Errorf("refresh credential at github: %w", errors.Join(admin.ErrUnavailable, errors.New("dial refused")))
	changes := e.countChanges()

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	mc := e.store.conns[c.ID]
	plain, _ := e.box.Open(mc.cred.KeyID, mc.cred.Data)
	if mc.c.Status != admin.StatusActive || string(plain) != "plain-inst1" || !mc.c.RefreshAfter.After(e.now) || *changes != 0 {
		t.Fatalf("status=%s plain=%q refreshAfter=%v changes=%d", mc.c.Status, plain, mc.c.RefreshAfter, *changes)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "dial refused") || !strings.Contains(logs, c.ID.String()) {
		t.Fatalf("logs = %s", logs)
	}
}

func TestRefreshDueSkipsConnectionsNotDue(t *testing.T) {
	e := newConnEnv(t)
	if _, err := e.connect(t, "linear", "ws1"); err != nil { // refresh_after unset: never due
		t.Fatal(err)
	}
	changes := e.countChanges()

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	if len(e.conn.refreshedWith) != 0 || *changes != 0 {
		t.Fatalf("refreshed %d, changes %d", len(e.conn.refreshedWith), *changes)
	}
}

func TestRefreshKeepsCredentialOfReconnectDuringRefresh(t *testing.T) {
	e := newConnEnv(t)
	c := e.dueConnection(t, "linear", "ws1")
	e.conn.refreshErr = fmt.Errorf("refresh credential at linear: %w", admin.ErrCredentialRejected)
	e.conn.onRefresh = func() { // the admin reconnects while the old token is being refreshed
		e.conn.onRefresh = nil
		if _, err := e.svc.Authorize(ctx, "acme", "linear"); err != nil {
			t.Error(err)
		}
		e.conn.completed = admin.Completed{Data: []byte("plain-reconnected"), Label: "WS1", ExternalID: "ws1"}
		if _, err := e.svc.Callback(ctx, "linear", map[string]string{"state": e.conn.state}); err != nil {
			t.Error(err)
		}
	}

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	mc := e.store.conns[c.ID]
	plain, err := e.box.Open(mc.cred.KeyID, mc.cred.Data)
	if err != nil || string(plain) != "plain-reconnected" || mc.c.Status != admin.StatusActive {
		t.Fatalf("plain=%q err=%v status=%s", plain, err, mc.c.Status)
	}
}

func TestRefreshGivesUpOnHangingAdapter(t *testing.T) {
	e := newConnEnv(t)
	c := e.dueConnection(t, "linear", "ws1")
	admin.SetTimeouts(e.svc, 10*time.Millisecond, time.Minute)
	e.conn.block = true

	if err := e.svc.RefreshDue(ctx); err != nil {
		t.Fatal(err)
	}

	if mc := e.store.conns[c.ID]; mc.c.Status != admin.StatusActive || !strings.Contains(e.logs.String(), "deadline exceeded") {
		t.Fatalf("status=%s logs=%s", mc.c.Status, e.logs)
	}
}
