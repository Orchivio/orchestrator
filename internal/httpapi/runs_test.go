package httpapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/router"
)

var runsBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

// runsEnv has tenant acme with two projects; runs are added via e.st.
func runsEnv(t *testing.T) (e *env, projectA, projectB string) {
	t.Helper()
	e = newEnv(t)
	profile := e.seedTenantAndProfile()
	create := func(key string) string {
		body := strings.Replace(projectBody(profile, "o/x"), `"ACME"`, `"`+key+`"`, 1)
		rec := e.do("POST", "/api/v1/tenants/acme/projects", body)
		if rec.Code != http.StatusCreated {
			t.Fatal(rec.Code, rec.Body.String())
		}
		return decode(t, rec)["id"].(string)
	}
	return e, create("RESA"), create("RESB")
}

func (e *env) addRun(project, ticket, state string, created time.Time) router.Run {
	r := router.Run{ID: uuid.New(), Tenant: "acme", TicketRef: ticket, State: state, CreatedAt: created, UpdatedAt: created}
	if project != "" {
		r.ProjectID = uuid.MustParse(project)
	}
	e.st.addRun(r)
	return r
}

func runsPath(project string) string { return "/api/v1/tenants/acme/projects/" + project + "/runs" }

type runList struct {
	Runs []struct{ ID, TicketRef, State string }
	Next string
}

func listRuns(t *testing.T, e *env, path string) runList {
	t.Helper()
	rec := e.do("GET", path, "")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var out runList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestListRunsNewestFirstWithFilters(t *testing.T) {
	e, a, _ := runsEnv(t)
	old := e.addRun(a, "RES-1", "running", runsBase)
	mid := e.addRun(a, "RES-2", "succeeded", runsBase.Add(time.Minute))
	newest := e.addRun(a, "RES-1", "failed", runsBase.Add(2*time.Minute))

	all := listRuns(t, e, runsPath(a))
	if len(all.Runs) != 3 || all.Runs[0].ID != newest.ID.String() || all.Runs[1].ID != mid.ID.String() || all.Runs[2].ID != old.ID.String() {
		t.Errorf("order = %+v", all.Runs)
	}
	if got := listRuns(t, e, runsPath(a)+"?ticket=RES-1"); len(got.Runs) != 2 || got.Runs[0].ID != newest.ID.String() || got.Runs[1].ID != old.ID.String() {
		t.Errorf("ticket filter = %+v", got.Runs)
	}
	if got := listRuns(t, e, runsPath(a)+"?state=failed"); len(got.Runs) != 1 || got.Runs[0].ID != newest.ID.String() {
		t.Errorf("state filter = %+v", got.Runs)
	}
}

func TestEmptyRunListIsEmptyArray(t *testing.T) {
	e, a, _ := runsEnv(t)
	rec := e.do("GET", runsPath(a), "")
	if body := strings.TrimSpace(rec.Body.String()); body != `{"runs":[]}` {
		t.Errorf("body = %s", body)
	}
}

func TestRunPagesAreStableWithEqualTimestamps(t *testing.T) {
	e, a, _ := runsEnv(t)
	for range 5 {
		e.addRun(a, "RES-1", "running", runsBase)
	}
	seen, sizes, path := map[string]bool{}, []int{}, runsPath(a)+"?limit=2"
	for path != "" {
		page := listRuns(t, e, path)
		sizes = append(sizes, len(page.Runs))
		for _, r := range page.Runs {
			if seen[r.ID] {
				t.Fatalf("run %s listed twice", r.ID)
			}
			seen[r.ID] = true
		}
		path = ""
		if page.Next != "" {
			path = runsPath(a) + "?limit=2&before=" + page.Next
		} else if len(page.Runs) != 1 {
			t.Errorf("last page has %d runs", len(page.Runs))
		}
	}
	if len(seen) != 5 || len(sizes) != 3 || sizes[0] != 2 || sizes[1] != 2 || sizes[2] != 1 {
		t.Errorf("seen %d runs, pages %v", len(seen), sizes)
	}
	rec := e.do("GET", runsPath(a), "")
	if strings.Contains(rec.Body.String(), `"next"`) {
		t.Errorf("single page must not carry next: %s", rec.Body)
	}
}

func TestRunsWithoutProjectIDAreIgnored(t *testing.T) {
	e, a, _ := runsEnv(t)
	orphan := e.addRun("", "RES-1", "running", runsBase)
	if got := listRuns(t, e, runsPath(a)); len(got.Runs) != 0 {
		t.Errorf("list = %+v", got.Runs)
	}
	if rec := e.do("GET", runsPath(a)+"/"+orphan.ID.String(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("code = %d, body %s", rec.Code, rec.Body)
	}
}

func TestGetRunOfOtherProjectIs404(t *testing.T) {
	e, a, b := runsEnv(t)
	r := e.addRun(a, "RES-1", "running", runsBase)
	if rec := e.do("GET", runsPath(b)+"/"+r.ID.String(), ""); rec.Code != http.StatusNotFound {
		t.Errorf("via project B: code = %d", rec.Code)
	}
	if rec := e.do("GET", runsPath(a)+"/"+r.ID.String(), ""); rec.Code != http.StatusOK {
		t.Errorf("via project A: code = %d", rec.Code)
	}
}

func TestRunsRequireTokenAndTenantScope(t *testing.T) {
	instance, tenant := tenantAdminEnv(t)
	instance.st.addRun(router.Run{ID: uuid.New(), Tenant: "acme", ProjectID: uuid.New()})
	pid := decode(t, tenant.do("POST", "/api/v1/tenants/acme/profiles", profileBody))["id"].(string)
	rec := tenant.do("POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/x"))
	project := decode(t, rec)["id"].(string)

	req := httptest.NewRequest("GET", runsPath(project), nil)
	anon := httptest.NewRecorder()
	instance.h.ServeHTTP(anon, req)
	if anon.Code != http.StatusUnauthorized {
		t.Errorf("no token: code = %d", anon.Code)
	}
	if rec := tenant.do("GET", "/api/v1/tenants/other/projects/"+project+"/runs", ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign tenant: code = %d", rec.Code)
	}
	if rec := tenant.do("GET", runsPath(project), ""); rec.Code != http.StatusOK {
		t.Errorf("own tenant: code = %d", rec.Code)
	}
	if rec := instance.do("GET", runsPath(project), ""); rec.Code != http.StatusOK {
		t.Errorf("instance admin: code = %d", rec.Code)
	}
}

func TestInvalidLimitOrCursorIs400(t *testing.T) {
	e, a, _ := runsEnv(t)
	for query, field := range map[string]string{"limit=0": "limit", "limit=201": "limit", "limit=abc": "limit", "before=garbage": "before"} {
		rec := e.do("GET", runsPath(a)+"?"+query, "")
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Content-Type") != "application/problem+json" ||
			!strings.Contains(rec.Body.String(), `"field":"`+field+`"`) {
			t.Errorf("%s: code = %d, body %s", query, rec.Code, rec.Body)
		}
	}
}

func TestGetRunReturnsTimelineShape(t *testing.T) {
	e, a, _ := runsEnv(t)
	at := runsBase.Add(time.Second)
	r := e.addRun(a, "RES-1", "succeeded", runsBase)
	e.st.events[r.ID] = []admin.RunEvent{{Type: "message", Payload: json.RawMessage(`{"text":"hi"}`), At: at}}
	empty := e.addRun(a, "RES-2", "running", runsBase)

	rec := e.do("GET", runsPath(a)+"/"+r.ID.String(), "")
	var out struct {
		Run    struct{ ID, TicketRef, State string }
		Events []struct {
			Type    string
			Payload struct{ Text string }
			At      time.Time
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
		t.Fatal(rec.Code, err, rec.Body.String())
	}
	if out.Run.ID != r.ID.String() || out.Run.TicketRef != "RES-1" || out.Run.State != "succeeded" ||
		len(out.Events) != 1 || out.Events[0].Type != "message" || out.Events[0].Payload.Text != "hi" || !out.Events[0].At.Equal(at) {
		t.Errorf("body = %s", rec.Body)
	}
	var raw map[string]json.RawMessage
	_ = json.Unmarshal(e.do("GET", runsPath(a)+"/"+empty.ID.String(), "").Body.Bytes(), &raw)
	if string(raw["events"]) != "[]" {
		t.Errorf("events without any = %s", raw["events"])
	}
}
