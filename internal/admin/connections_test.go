package admin_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/secretbox"
)

const testKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=" // 32 bytes 0..31

// fakeConnector plays the adapters; completed is what CompleteConnect returns.
type fakeConnector struct {
	mu                    sync.Mutex
	state, redirect, kind string
	completed             admin.Completed
	completeErr           error
	completeCalls         int
	disconnected          [][]byte
}

func (f *fakeConnector) BeginConnect(_ context.Context, adapter, kind, tenant, redirectURI, state string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state, f.redirect, f.kind = state, redirectURI, kind
	return "https://provider.example/authorize?state=" + state, nil
}

func (f *fakeConnector) CompleteConnect(_ context.Context, adapter, kind, redirectURI string, query map[string]string) (admin.Completed, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completeCalls++
	f.redirect, f.kind = redirectURI, kind
	return f.completed, f.completeErr
}

func (f *fakeConnector) Refresh(context.Context, string, string, []byte, *time.Time) (admin.Completed, error) {
	return admin.Completed{}, errors.New("not used")
}

func (f *fakeConnector) Disconnect(_ context.Context, adapter, kind string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnected = append(f.disconnected, data)
	return errors.New("provider down") // best effort: must not block the delete
}

func (f *fakeConnector) CheckAccess(context.Context, string, string, [][]byte, []string) ([]string, error) {
	return nil, errors.New("not used")
}

type connEnv struct {
	svc   *admin.Service
	store *memStore
	conn  *fakeConnector
	box   *secretbox.Box
	now   time.Time
	logs  *bytes.Buffer
}

func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	box, err := secretbox.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	e := &connEnv{store: newMemStore(), conn: &fakeConnector{}, box: box, now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC), logs: &bytes.Buffer{}}
	adapters := map[string]string{"linear": "adapter-linear:7101", "github": "adapter-github:7201"}
	e.svc = admin.NewService(e.store, adapters, box, e.conn, "http://localhost:8088", func() time.Time { return e.now }, uuid.New,
		slog.New(slog.NewTextHandler(e.logs, nil)))
	seed(t, e.svc)
	return e
}

// connect runs authorize + callback for adapter and returns the callback result.
func (e *connEnv) connect(t *testing.T, adapter, externalID string) (admin.Connection, error) {
	t.Helper()
	if _, err := e.svc.Authorize(ctx, "acme", adapter); err != nil {
		t.Fatal(err)
	}
	e.conn.completed = admin.Completed{Data: []byte("plain-" + externalID), Label: "Label " + externalID, ExternalID: externalID}
	return e.svc.Callback(ctx, adapter, map[string]string{"code": "c0de", "state": e.conn.state})
}

func TestAuthorizeStoresHashedStateAndReturnsAdapterURL(t *testing.T) {
	e := newConnEnv(t)
	url, err := e.svc.Authorize(ctx, "acme", "github")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://provider.example/authorize?state="+e.conn.state || e.conn.redirect != "http://localhost:8088/oauth/callback/github" ||
		e.conn.kind != admin.KindCodeHost || len(e.conn.state) < 40 {
		t.Fatalf("url=%q redirect=%q kind=%q state len %d", url, e.conn.redirect, e.conn.kind, len(e.conn.state))
	}
	sum := sha256.Sum256([]byte(e.conn.state))
	st, ok := e.store.states[string(sum[:])]
	if !ok || len(e.store.states) != 1 || st.adapter != "github" || st.kind != admin.KindCodeHost || !st.expires.Equal(e.now.Add(10*time.Minute)) {
		t.Fatalf("states = %+v", e.store.states)
	}
	if _, raw := e.store.states[e.conn.state]; raw {
		t.Fatal("state stored in plain")
	}
}

func TestAuthorizeRejectsAdapterNotAssignedToTenant(t *testing.T) {
	e := newConnEnv(t)
	_, err := e.svc.Authorize(ctx, "acme", "jira")
	var ve *admin.ValidationError
	if !errors.As(err, &ve) || !hasField(ve, "adapter") || len(e.store.states) != 0 {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.Authorize(ctx, "nope", "github"); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func TestCallbackCreatesConnection(t *testing.T) {
	e := newConnEnv(t)
	refresh := e.now.Add(12 * time.Hour)
	if _, err := e.svc.Authorize(ctx, "acme", "linear"); err != nil {
		t.Fatal(err)
	}
	e.conn.completed = admin.Completed{Data: []byte("plain-token"), RefreshAfter: &refresh, Label: "Acme WS", ExternalID: "ws1"}
	c, err := e.svc.Callback(ctx, "linear", map[string]string{"code": "c0de", "state": e.conn.state})
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != admin.KindTracker || c.Label != "Acme WS" || c.ExternalID != "ws1" || c.Status != admin.StatusActive ||
		c.TenantSlug != "acme" || !c.RefreshAfter.Equal(refresh) || e.conn.redirect != "http://localhost:8088/oauth/callback/linear" {
		t.Fatalf("conn = %+v redirect %q", c, e.conn.redirect)
	}
	cred := e.store.conns[c.ID].cred
	plain, err := e.box.Open(cred.KeyID, cred.Data)
	if err != nil || string(plain) != "plain-token" || bytes.Contains(cred.Data, []byte("plain-token")) {
		t.Fatalf("cred %+v plain %q err %v", cred, plain, err)
	}
}

func TestCallbackRejectsReplayedExpiredOrForeignState(t *testing.T) {
	e := newConnEnv(t)
	if _, err := e.connect(t, "github", "inst1"); err != nil {
		t.Fatal(err)
	}
	replay := e.conn.state
	cases := map[string]func() (string, string){
		"replay": func() (string, string) { return "github", replay },
		"expired": func() (string, string) {
			if _, err := e.svc.Authorize(ctx, "acme", "github"); err != nil {
				t.Fatal(err)
			}
			e.now = e.now.Add(11 * time.Minute)
			return "github", e.conn.state
		},
		"foreign adapter": func() (string, string) {
			if _, err := e.svc.Authorize(ctx, "acme", "linear"); err != nil {
				t.Fatal(err)
			}
			return "github", e.conn.state
		},
		"missing": func() (string, string) { return "github", "" },
	}
	for name, prepare := range cases {
		calls := e.conn.completeCalls
		adapter, state := prepare()
		_, err := e.svc.Callback(ctx, adapter, map[string]string{"code": "c0de", "state": state})
		if !errors.Is(err, admin.ErrUnauthorized) || e.conn.completeCalls != calls {
			t.Errorf("%s: err = %v, complete calls %d -> %d", name, err, calls, e.conn.completeCalls)
		}
	}
	if conns, _ := e.svc.ListConnections(ctx, "acme"); len(conns) != 1 {
		t.Fatalf("connections = %+v", conns)
	}
}

func TestCallbackPassesAdapterErrorsThrough(t *testing.T) {
	e := newConnEnv(t)
	if _, err := e.svc.Authorize(ctx, "acme", "github"); err != nil {
		t.Fatal(err)
	}
	e.conn.completeErr = &admin.NotConnectedError{Reason: "an org owner must connect"}
	_, err := e.svc.Callback(ctx, "github", map[string]string{"state": e.conn.state})
	var nc *admin.NotConnectedError
	if !errors.Is(err, admin.ErrNotConnected) || !errors.As(err, &nc) || nc.Reason != "an org owner must connect" {
		t.Fatalf("err = %v", err)
	}
}

func TestCallbackExclusiveAcrossTenants(t *testing.T) {
	e := newConnEnv(t)
	if _, err := e.connect(t, "github", "inst1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTenant(ctx, admin.Tenant{Slug: "beta", TrackerAdapter: "linear", CodeHostAdapter: "github"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authorize(ctx, "beta", "github"); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.Callback(ctx, "github", map[string]string{"state": e.conn.state})
	if conns, _ := e.svc.ListConnections(ctx, "beta"); !errors.Is(err, admin.ErrConflict) || len(conns) != 0 {
		t.Fatalf("err = %v conns %+v", err, conns)
	}
}

func TestCallbackReconnectMarksActive(t *testing.T) {
	e := newConnEnv(t)
	first, err := e.connect(t, "linear", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.UpdateConnectionCredential(ctx, first.ID, e.store.conns[first.ID].cred, nil, admin.StatusNeedsReauth); err != nil {
		t.Fatal(err)
	}
	again, err := e.connect(t, "linear", "ws1")
	if err != nil || again.ID != first.ID || again.Status != admin.StatusActive {
		t.Fatalf("again = %+v err %v", again, err)
	}
}

func TestTrackerSwitchBlockedWhileProjectsExist(t *testing.T) {
	e := newConnEnv(t)
	if _, err := e.connect(t, "linear", "ws1"); err != nil {
		t.Fatal(err)
	}
	prof, err := e.svc.GetProfile(ctx, "acme", e.store.profileID())
	if err != nil {
		t.Fatal(err)
	}
	proj, err := e.svc.CreateProject(ctx, admin.Project{TenantSlug: "acme", Name: "Res", TrackerKey: "RES", ProfileID: prof.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.connect(t, "linear", "ws2"); !errors.Is(err, admin.ErrInUse) {
		t.Fatalf("with project: err = %v", err)
	}
	if err := e.svc.DeleteProject(ctx, "acme", proj.ID, 0); err != nil {
		t.Fatal(err)
	}
	c, err := e.connect(t, "linear", "ws2")
	conns, _ := e.svc.ListConnections(ctx, "acme")
	if err != nil || len(conns) != 1 || conns[0].ID != c.ID || conns[0].ExternalID != "ws2" {
		t.Fatalf("err = %v conns %+v", err, conns)
	}
}

func TestFailedTrackerSwitchKeepsOldConnection(t *testing.T) {
	e := newConnEnv(t)
	old, err := e.connect(t, "linear", "ws1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.CreateTenant(ctx, admin.Tenant{Slug: "beta", TrackerAdapter: "linear", CodeHostAdapter: "github"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Authorize(ctx, "beta", "linear"); err != nil {
		t.Fatal(err)
	}
	e.conn.completed = admin.Completed{Data: []byte("plain-ws2"), Label: "WS2", ExternalID: "ws2"}
	if _, err := e.svc.Callback(ctx, "linear", map[string]string{"state": e.conn.state}); err != nil {
		t.Fatal(err)
	}
	changes := 0
	e.svc.OnChange(func(context.Context) error { changes++; return nil })
	if _, err := e.connect(t, "linear", "ws2"); !errors.Is(err, admin.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	conns, _ := e.svc.ListConnections(ctx, "acme")
	if len(conns) != 1 || conns[0].ID != old.ID || conns[0].Status != admin.StatusActive || changes != 0 {
		t.Fatalf("conns = %+v changes %d", conns, changes)
	}
}

func (m *memStore) profileID() uuid.UUID {
	for id := range m.profiles {
		return id
	}
	return uuid.Nil
}

func TestDeleteConnectionCallsDisconnectAndSoftDeletes(t *testing.T) {
	e := newConnEnv(t)
	c, err := e.connect(t, "github", "inst1")
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	e.svc.OnChange(func(context.Context) error { changes++; return nil })
	if err := e.svc.DeleteConnection(ctx, "acme", c.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.conn.disconnected) != 1 || string(e.conn.disconnected[0]) != "plain-inst1" || !e.store.conns[c.ID].dead || changes != 1 {
		t.Fatalf("disconnected %q dead %v changes %d", e.conn.disconnected, e.store.conns[c.ID].dead, changes)
	}
	if strings.Contains(e.logs.String(), "plain-inst1") || !strings.Contains(e.logs.String(), "provider down") {
		t.Fatalf("logs = %s", e.logs)
	}
	if _, err := e.svc.GetConnection(ctx, "acme", c.ID); !errors.Is(err, admin.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestConnectionsUnavailableWithoutBoxOrConnector(t *testing.T) {
	svc := admin.NewService(newMemStore(), nil, nil, nil, "http://localhost:8088", time.Now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := svc.Authorize(ctx, "acme", "github"); !errors.Is(err, admin.ErrUnavailable) {
		t.Fatalf("authorize: %v", err)
	}
	if _, err := svc.Callback(ctx, "github", map[string]string{"state": "x"}); !errors.Is(err, admin.ErrUnavailable) {
		t.Fatalf("callback: %v", err)
	}
}
