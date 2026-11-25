package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/httpapi"
	"github.com/orchivio/orchestrator/internal/secretbox"
)

const testKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

type fakeConnector struct {
	mu          sync.Mutex
	state       string
	completed   admin.Completed
	completeErr error
	beginErr    error
}

func (f *fakeConnector) BeginConnect(_ context.Context, _, _, _, _, state string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = state
	return "https://provider.example/authorize?state=" + state, f.beginErr
}

func (f *fakeConnector) CompleteConnect(context.Context, string, string, string, map[string]string) (admin.Completed, error) {
	return f.completed, f.completeErr
}

func (f *fakeConnector) Refresh(context.Context, string, string, []byte, *time.Time) (admin.Completed, error) {
	return admin.Completed{}, errors.New("not used")
}

func (f *fakeConnector) Disconnect(context.Context, string, string, []byte) error { return nil }

func (f *fakeConnector) CheckAccess(context.Context, string, string, [][]byte, []string) ([]string, error) {
	return nil, nil // everything accessible
}

type connEnv struct {
	*env
	store *memStore
	conn  *fakeConnector
	logs  *bytes.Buffer
}

// newConnEnv serves the admin API and the callback page like main.go, with tenants acme and beta.
func newConnEnv(t *testing.T) *connEnv {
	t.Helper()
	box, err := secretbox.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	e := &connEnv{store: newMemStore(), conn: &fakeConnector{}, logs: &bytes.Buffer{}}
	log := slog.New(slog.NewTextHandler(e.logs, nil))
	svc := admin.NewService(e.store, map[string]string{"linear": "a:1", "github": "b:2"}, box, e.conn, "http://localhost:8088",
		time.Now, uuid.New, log)
	_, plain, err := svc.IssueToken(t.Context(), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", httpapi.NewAdmin(svc, log))
	mux.Handle("GET /oauth/callback/{adapter}", httpapi.NewCallback(svc, log))
	e.env = &env{t: t, h: mux, token: plain}
	for _, slug := range []string{"acme", "beta"} {
		body := strings.Replace(tenantBody, "acme", slug, 1)
		if rec := e.do("POST", "/api/v1/tenants", body); rec.Code != 201 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	return e
}

// connect authorizes tenant at adapter and calls back with code; it returns the page.
func (e *connEnv) connect(tenant, adapter, externalID string) *httptest.ResponseRecorder {
	e.t.Helper()
	if rec := e.do("POST", "/api/v1/tenants/"+tenant+"/connections/"+adapter+"/authorize", ""); rec.Code != 200 ||
		decode(e.t, rec)["url"] != "https://provider.example/authorize?state="+e.conn.state {
		e.t.Fatal(rec.Code, rec.Body.String())
	}
	e.conn.completed = admin.Completed{Data: []byte("plain-secret-" + externalID), Label: "<b>" + externalID + "</b>", ExternalID: externalID}
	return e.callback(adapter, e.conn.state)
}

func (e *connEnv) callback(adapter, state string) *httptest.ResponseRecorder {
	q := url.Values{"code": {"the-code-123"}, "state": {state}}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, httptest.NewRequest("GET", "/oauth/callback/"+adapter+"?"+q.Encode(), nil))
	return rec
}

func TestCallbackPageShowsEscapedLabel(t *testing.T) {
	e := newConnEnv(t)
	rec := e.connect("acme", "github", "inst1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Verbunden: &lt;b&gt;inst1&lt;/b&gt; (Mandant acme)") ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestCallbackExclusiveAcrossTenants(t *testing.T) {
	e := newConnEnv(t)
	if rec := e.connect("acme", "github", "inst1"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec := e.connect("beta", "github", "inst1")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "Bereits mit einem anderen Mandanten verbunden") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestCallbackErrorPages(t *testing.T) {
	e := newConnEnv(t)
	e.conn.completeErr = &admin.NotConnectedError{Reason: "Ask an <org owner>"}
	rec := e.connect("acme", "github", "inst1")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Ask an &lt;org owner&gt;") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	e.conn.completeErr = &admin.RejectedError{Reason: "only an organization <admin> can connect this installation"}
	rec = e.connect("acme", "github", "inst1")
	if rec.Code != 403 || !strings.Contains(rec.Body.String(), "Verbindung fehlgeschlagen: only an organization &lt;admin&gt; can connect") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	e.conn.completeErr = errors.Join(admin.ErrCredentialRejected, errors.New("secret detail"))
	rec = e.connect("acme", "github", "inst1")
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "Verbindung fehlgeschlagen") || strings.Contains(rec.Body.String(), "secret detail") {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestConnectionsAPI(t *testing.T) {
	e := newConnEnv(t)
	rec := e.do("POST", "/api/v1/tenants/acme/connections/jira/authorize", "")
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"field":"adapter"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.do("POST", "/api/v1/tenants/nope/connections/github/authorize", ""); rec.Code != 404 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	e.connect("acme", "github", "inst1")
	rec = e.do("GET", "/api/v1/tenants/acme/connections", "")
	var list []map[string]any
	if err := jsonUnmarshal(rec, &list); err != nil || rec.Code != 200 || len(list) != 1 || list[0]["kind"] != "codehost" ||
		list[0]["adapter"] != "github" || list[0]["status"] != "active" || list[0]["label"] != "<b>inst1</b>" || list[0]["createdAt"] == nil {
		t.Fatal(rec.Code, rec.Body.String())
	}
	id := list[0]["id"].(string)
	if rec := e.do("GET", "/api/v1/tenants/acme/connections/"+id, ""); rec.Code != 200 || decode(t, rec)["id"] != id {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/v1/tenants/beta/connections/"+id, ""); rec.Code != 404 {
		t.Fatal("foreign tenant:", rec.Code)
	}
	if rec := e.do("DELETE", "/api/v1/tenants/acme/connections/"+id, ""); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.do("GET", "/api/v1/tenants/acme/connections/"+id, ""); rec.Code != 404 {
		t.Fatal("after delete:", rec.Code)
	}
}

func TestAuthorizeAdapterFailureIs502WithoutDetail(t *testing.T) {
	e := newConnEnv(t)
	e.conn.beginErr = errors.New("adapter said secret-detail")
	rec := e.do("POST", "/api/v1/tenants/acme/connections/github/authorize", "")
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "secret-detail") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	e.conn.beginErr = errors.Join(admin.ErrUnavailable, errors.New("dial"))
	if rec := e.do("POST", "/api/v1/tenants/acme/connections/github/authorize", ""); rec.Code != 503 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestConnectionEndpointsUnavailableWithoutBox(t *testing.T) {
	e := newEnvWith(t, admin.NewService(newMemStore(), map[string]string{"linear": "a:1", "github": "b:2"}, nil, nil, "",
		time.Now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil))))
	e.seedTenantAndProfile()
	for _, r := range []struct{ method, path string }{
		{"POST", "/api/v1/tenants/acme/connections/github/authorize"},
		{"GET", "/api/v1/tenants/acme/connections"},
		{"GET", "/api/v1/tenants/acme/connections/" + uuid.NewString()},
		{"DELETE", "/api/v1/tenants/acme/connections/" + uuid.NewString()},
	} {
		if rec := e.do(r.method, r.path, ""); rec.Code != 503 || rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("%s %s = %d", r.method, r.path, rec.Code)
		}
	}
}

func TestConnectionsNeverExposeCredential(t *testing.T) {
	e := newConnEnv(t)
	page := e.connect("acme", "github", "inst1")
	state := e.conn.state
	replay := e.callback("github", state) // logged failure path
	if replay.Code != 400 || !strings.Contains(replay.Body.String(), "Verbindung fehlgeschlagen") {
		t.Fatal(replay.Code, replay.Body.String())
	}
	e.do("POST", "/api/v1/tenants/acme/connections/linear/authorize", "")
	foreign := e.callback("github", e.conn.state)
	list := e.do("GET", "/api/v1/tenants/acme/connections", "")
	var items []map[string]any
	if err := jsonUnmarshal(list, &items); err != nil || len(items) != 1 {
		t.Fatal(list.Body.String())
	}
	item := e.do("GET", "/api/v1/tenants/acme/connections/"+items[0]["id"].(string), "")
	var sealed []byte
	for _, mc := range e.store.conns {
		sealed = mc.cred.Data
	}
	secrets := []string{"plain-secret-inst1", string(sealed), base64.StdEncoding.EncodeToString(sealed), "the-code-123", state}
	for name, text := range map[string]string{"page": page.Body.String(), "replay": replay.Body.String(), "foreign": foreign.Body.String(),
		"list": list.Body.String(), "item": item.Body.String(), "logs": e.logs.String()} {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("%s exposes %q", name, s)
			}
		}
	}
}

func jsonUnmarshal(rec *httptest.ResponseRecorder, dst any) error {
	return json.Unmarshal(rec.Body.Bytes(), dst)
}

func TestTrackerConnectionKeptWhileProjectsExist(t *testing.T) {
	e := newConnEnv(t)
	e.connect("acme", "linear", "ws1")
	e.connect("acme", "github", "inst1")
	prof := e.do("POST", "/api/v1/tenants/acme/profiles", profileBody)
	project := e.do("POST", "/api/v1/tenants/acme/projects", projectBody(decode(t, prof)["id"].(string), "o/x"))
	if project.Code != 201 {
		t.Fatal(project.Code, project.Body.String())
	}
	var tracker string
	for id, mc := range e.store.conns {
		if mc.c.Kind == admin.KindTracker {
			tracker = id.String()
		}
	}
	rec := e.do("DELETE", "/api/v1/tenants/acme/connections/"+tracker, "")
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "delete its projects first") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if err := e.store.DeleteConnection(t.Context(), "acme", uuid.MustParse(tracker)); err != nil {
		t.Fatal(err)
	}
	if rec := e.connect("acme", "linear", "ws2"); rec.Code != 409 {
		t.Fatal("other workspace after delete:", rec.Code, rec.Body.String())
	}
	if rec := e.do("DELETE", decodeLocation(project), "", "If-Match", `"1"`); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := e.connect("acme", "linear", "ws2"); rec.Code != 200 {
		t.Fatal("other workspace without projects:", rec.Code, rec.Body.String())
	}
}

func decodeLocation(rec *httptest.ResponseRecorder) string { return rec.Header().Get("Location") }

func TestCallbackAnswersOnlyGetAndKeepsStateOtherwise(t *testing.T) {
	e := newConnEnv(t)
	if rec := e.do("POST", "/api/v1/tenants/acme/connections/github/authorize", ""); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	e.conn.completed = admin.Completed{Data: []byte("plain"), Label: "inst1", ExternalID: "inst1"}
	q := url.Values{"code": {"c"}, "state": {e.conn.state}}.Encode()
	rec := httptest.NewRecorder() // a GET pattern also routes HEAD
	e.h.ServeHTTP(rec, httptest.NewRequest("HEAD", "/oauth/callback/github?"+q, nil))
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET" {
		t.Fatalf("HEAD: %d %v", rec.Code, rec.Header())
	}
	if rec := e.callback("github", e.conn.state); rec.Code != 200 {
		t.Fatal("state consumed by HEAD:", rec.Code, rec.Body.String())
	}
}
