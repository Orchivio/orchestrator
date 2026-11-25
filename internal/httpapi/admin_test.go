package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/httpapi"
	"github.com/orchivio/orchestrator/internal/secretbox"
)

const (
	tenantBody  = `{"slug":"acme","displayName":"Acme","trackerAdapter":"linear","codeHostAdapter":"github"}`
	profileBody = `{"name":"default","runtime":"podman","model":"m","instructions":"i","maxDuration":"20m"}`
)

type env struct {
	t     *testing.T
	st    *memStore
	h     http.Handler
	token string
}

// newEnv serves the admin API; tenant acme is connected to linear and github
// once created, and the adapters allow every key and repo.
func newEnv(t *testing.T) *env {
	t.Helper()
	box, err := secretbox.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	for _, c := range []admin.Connection{
		{Kind: admin.KindTracker, Adapter: "linear", ExternalID: "ws-acme"},
		{Kind: admin.KindCodeHost, Adapter: "github", ExternalID: "inst-acme"},
	} {
		c.ID, c.TenantSlug, c.Status = uuid.New(), "acme", admin.StatusActive
		sealed, err := box.Seal([]byte("plain"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.SaveConnection(t.Context(), c, admin.SealedCredential{KeyID: box.KeyID(), Data: sealed}); err != nil {
			t.Fatal(err)
		}
	}
	e := newEnvWith(t, admin.NewService(st, map[string]string{"linear": "a:1", "github": "b:2"}, box, &fakeConnector{}, "",
		time.Now, uuid.New, slog.New(slog.NewTextHandler(io.Discard, nil))))
	e.st = st
	return e
}

func newEnvWith(t *testing.T, svc *admin.Service) *env {
	t.Helper()
	_, plain, err := svc.IssueToken(t.Context(), "test", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, h: httpapi.NewAdmin(svc, slog.New(slog.NewTextHandler(io.Discard, nil))), token: plain}
}

func (e *env) do(method, path, body string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.token)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return m
}

func (e *env) seedTenantAndProfile() string {
	e.t.Helper()
	if rec := e.do("POST", "/api/v1/tenants", tenantBody); rec.Code != 201 {
		e.t.Fatal(rec.Code, rec.Body.String())
	}
	rec := e.do("POST", "/api/v1/tenants/acme/profiles", profileBody)
	if rec.Code != 201 {
		e.t.Fatal(rec.Code, rec.Body.String())
	}
	return decode(e.t, rec)["id"].(string)
}

func projectBody(profileID, defaultRepo string) string {
	return `{"name":"P","trackerKey":"ACME","profileId":"` + profileID + `","repos":[{"slug":"o/x","baseBranch":"main"}],` +
		`"classifier":{"defaultRepo":"` + defaultRepo + `"},"roles":{"questions":[],"tasks":[]},"maxDiffLines":500,"milestones":[]}`
}

func TestCreateAndReadProject(t *testing.T) {
	e := newEnv(t)
	pid := e.seedTenantAndProfile()
	rec := e.do("POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/x"))
	if rec.Code != 201 || rec.Header().Get("ETag") != `"1"` {
		t.Fatal(rec.Code, rec.Header(), rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/api/v1/tenants/acme/projects/") {
		t.Fatal(loc)
	}
	got := e.do("GET", loc, "")
	if got.Code != 200 || got.Body.String() != rec.Body.String() {
		t.Fatalf("%d\n%s\n%s", got.Code, got.Body.String(), rec.Body.String())
	}
	if decode(t, got)["profileId"] != pid {
		t.Fatal(got.Body.String())
	}
}

func TestPutWithStaleIfMatchIs412(t *testing.T) {
	e := newEnv(t)
	body := `{"displayName":"New","trackerAdapter":"linear","codeHostAdapter":"github"}`
	e.do("POST", "/api/v1/tenants", tenantBody)
	if rec := e.do("PUT", "/api/v1/tenants/acme", body, "If-Match", `"1"`); rec.Code != 200 || rec.Header().Get("ETag") != `"2"` {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec := e.do("PUT", "/api/v1/tenants/acme", body, "If-Match", `"1"`)
	if rec.Code != 412 || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal(rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestValidationErrorIs422WithFields(t *testing.T) {
	e := newEnv(t)
	pid := e.seedTenantAndProfile()
	rec := e.do("POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/other"))
	if rec.Code != 422 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	errs := decode(t, rec)["errors"].([]any)
	if errs[0].(map[string]any)["field"] != "classifier.defaultRepo" {
		t.Fatal(rec.Body.String())
	}
}

func TestInvalidMaxDurationIs422(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/api/v1/tenants", tenantBody)
	rec := e.do("POST", "/api/v1/tenants/acme/profiles", `{"name":"x","runtime":"podman","maxDuration":"soon"}`)
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"maxDuration"`) {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestUnknownFieldIs400(t *testing.T) {
	e := newEnv(t)
	for _, body := range []string{`{"slug":"a","bogus":1}`, `{`} {
		if rec := e.do("POST", "/api/v1/tenants", body); rec.Code != 400 {
			t.Fatal(body, rec.Code, rec.Body.String())
		}
	}
}

func TestNotFoundIs404(t *testing.T) {
	e := newEnv(t)
	if rec := e.do("GET", "/api/v1/tenants/nope", ""); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.do("GET", "/api/v1/tenants/nope/profiles/not-a-uuid", ""); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}

func TestDeleteInUseIs409(t *testing.T) {
	e := newEnv(t)
	pid := e.seedTenantAndProfile()
	e.do("POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/x"))
	if rec := e.do("DELETE", "/api/v1/tenants/acme/profiles/"+pid, ""); rec.Code != 409 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestDeleteReturns204(t *testing.T) {
	e := newEnv(t)
	e.do("POST", "/api/v1/tenants", tenantBody)
	if rec := e.do("DELETE", "/api/v1/tenants/acme", ""); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestCreateTokenReturnsPlaintextOnce(t *testing.T) {
	e := newEnv(t)
	rec := e.do("POST", "/api/v1/tokens", `{"name":"ci","expiresIn":"1h"}`)
	if rec.Code != 201 || !strings.HasPrefix(decode(t, rec)["token"].(string), "orv_") {
		t.Fatal(rec.Code, rec.Body.String())
	}
	list := e.do("GET", "/api/v1/tokens", "")
	if list.Code != 200 || strings.Contains(list.Body.String(), `"token"`) {
		t.Fatal(list.Code, list.Body.String())
	}
	id := decode(t, rec)["id"].(string)
	if del := e.do("DELETE", "/api/v1/tokens/"+id, ""); del.Code != 204 {
		t.Fatal(del.Code)
	}
}

func TestEveryRouteRequiresAuth(t *testing.T) {
	e := newEnv(t)
	for _, r := range httpapi.AdminRoutes {
		path := strings.NewReplacer("{slug}", "acme", "{id}", uuid.NewString()).Replace(r.Path)
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, httptest.NewRequest(r.Method, "/api/v1"+path, nil))
		if rec.Code != 401 {
			t.Errorf("%s %s: %d", r.Method, r.Path, rec.Code)
		}
	}
}

func TestMinimalProjectReturnsEmptyCollectionsNotNull(t *testing.T) {
	e := newEnv(t)
	pid := e.seedTenantAndProfile()
	body := `{"name":"P","trackerKey":"ACME","profileId":"` + pid + `","repos":[{"slug":"o/x","baseBranch":"main"}],"classifier":{"defaultRepo":"o/x"}}`
	rec := e.do("POST", "/api/v1/tenants/acme/projects", body)
	got := e.do("GET", rec.Header().Get("Location"), "").Body.String()
	for _, want := range []string{`"taskLabels":[]`, `"questionLabels":[]`, `"repoLabels":{}`, `"allowedPaths":[]`,
		`"users":[]`, `"questions":[]`, `"tasks":[]`, `"milestones":[]`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}

func TestChildrenOfDeletedTenantAre404(t *testing.T) {
	e := newEnv(t)
	pid := e.seedTenantAndProfile()
	loc := e.do("POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/x")).Header().Get("Location")
	if rec := e.do("DELETE", "/api/v1/tenants/acme", ""); rec.Code != 204 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/v1/tenants/acme/projects", "/api/v1/tenants/acme/profiles", loc,
		"/api/v1/tenants/never/projects"} {
		if rec := e.do("GET", path, ""); rec.Code != 404 {
			t.Errorf("GET %s = %d", path, rec.Code)
		}
	}
	if rec := e.do("PUT", loc, projectBody(pid, "o/x")); rec.Code != 404 {
		t.Errorf("PUT = %d", rec.Code)
	}
}

func TestBadIDWithBadIfMatchWritesOneProblem(t *testing.T) {
	e := newEnv(t)
	e.seedTenantAndProfile()
	for _, c := range [][2]string{{"PUT", "profiles"}, {"DELETE", "profiles"}, {"PUT", "projects"}, {"DELETE", "projects"}} {
		rec := e.do(c[0], "/api/v1/tenants/acme/"+c[1]+"/not-a-uuid", `{}`, "If-Match", "bogus")
		dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
		var first, second map[string]any
		if rec.Code != 404 || dec.Decode(&first) != nil || dec.Decode(&second) != io.EOF {
			t.Errorf("%s %s: %d %q", c[0], c[1], rec.Code, rec.Body.String())
		}
	}
}
