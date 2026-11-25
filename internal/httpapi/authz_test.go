package httpapi_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

const otherTenantBody = `{"slug":"other","displayName":"Other","trackerAdapter":"linear","codeHostAdapter":"github"}`

// tenantAdminEnv seeds tenants acme and other as instance admin and returns
// the instance env plus an env acting with an acme tenant-admin token.
func tenantAdminEnv(t *testing.T) (instance, tenant *env) {
	t.Helper()
	instance = newEnv(t)
	for _, b := range []string{tenantBody, otherTenantBody} {
		if rec := instance.do("POST", "/api/v1/tenants", b); rec.Code != http.StatusCreated {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	rec := instance.do("POST", "/api/v1/tokens", `{"name":"acme-admin","tenant":"acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatal(rec.Code, rec.Body.String())
	}
	tenant = &env{t: t, h: instance.h, token: decode(t, rec)["token"].(string)}
	return instance, tenant
}

func tokenIDs(t *testing.T, e *env) map[string]string {
	t.Helper()
	rec := e.do("GET", "/api/v1/tokens", "")
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); rec.Code != http.StatusOK || err != nil {
		t.Fatal(rec.Code, err, rec.Body.String())
	}
	ids := map[string]string{}
	for _, it := range items {
		ids[it["name"].(string)] = it["id"].(string)
	}
	return ids
}

func TestTenantAdminScope(t *testing.T) {
	instance, tenant := tenantAdminEnv(t)
	profile := tenant.do("POST", "/api/v1/tenants/acme/profiles", profileBody)
	if profile.Code != http.StatusCreated {
		t.Fatal(profile.Code, profile.Body.String())
	}
	pid := decode(t, profile)["id"].(string)
	const adaptersKept = `{"displayName":"Renamed","trackerAdapter":"linear","codeHostAdapter":"github"}`
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"own tenant", "GET", "/api/v1/tenants/acme", "", 200},
		{"other tenant", "GET", "/api/v1/tenants/other", "", 404},
		{"create tenant", "POST", "/api/v1/tenants", `{"slug":"new","displayName":"N","trackerAdapter":"linear","codeHostAdapter":"github"}`, 403},
		{"delete own tenant", "DELETE", "/api/v1/tenants/acme", "", 403},
		{"delete other tenant", "DELETE", "/api/v1/tenants/other", "", 404},
		{"rename own tenant", "PUT", "/api/v1/tenants/acme", adaptersKept, 200},
		{"rename own tenant without adapters", "PUT", "/api/v1/tenants/acme", `{"displayName":"Renamed"}`, 200},
		{"change tracker adapter", "PUT", "/api/v1/tenants/acme", `{"displayName":"R","trackerAdapter":"jira","codeHostAdapter":"github"}`, 403},
		{"change code host adapter", "PUT", "/api/v1/tenants/acme", `{"displayName":"R","trackerAdapter":"linear","codeHostAdapter":"gitlab"}`, 403},
		{"rename other tenant", "PUT", "/api/v1/tenants/other", adaptersKept, 404},
		{"own profiles", "GET", "/api/v1/tenants/acme/profiles", "", 200},
		{"own project create", "POST", "/api/v1/tenants/acme/projects", projectBody(pid, "o/x"), 201},
		{"own projects", "GET", "/api/v1/tenants/acme/projects", "", 200},
		{"own connections", "GET", "/api/v1/tenants/acme/connections", "", 200},
		{"other profiles", "GET", "/api/v1/tenants/other/profiles", "", 404},
		{"other projects", "GET", "/api/v1/tenants/other/projects", "", 404},
		{"other project create", "POST", "/api/v1/tenants/other/projects", projectBody(pid, "o/x"), 404},
		{"other connections", "GET", "/api/v1/tenants/other/connections", "", 404},
		{"other form", "GET", "/api/v1/tenants/other/connections/github/form", "", 404},
		{"other form connect", "POST", "/api/v1/tenants/other/connections/github/form", `{"fields":{}}`, 404},
		{"other authorize", "POST", "/api/v1/tenants/other/connections/linear/authorize", "", 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := tenant.do(c.method, c.path, c.body)
			if rec.Code != c.want {
				t.Fatalf("code = %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}

	t.Run("list tenants shows only own", func(t *testing.T) {
		rec := tenant.do("GET", "/api/v1/tenants", "")
		var items []map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 1 || items[0]["slug"] != "acme" {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	})

	if rec := instance.do("POST", "/api/v1/tokens", `{"name":"other-admin","tenant":"other"}`); rec.Code != http.StatusCreated {
		t.Fatal(rec.Code, rec.Body.String())
	}
	all := tokenIDs(t, instance)
	t.Run("tokens show only own tenant", func(t *testing.T) {
		got := tokenIDs(t, tenant)
		if len(got) != 1 || got["acme-admin"] == "" {
			t.Fatalf("tenant admin sees %v", got)
		}
	})
	t.Run("instance token invisible and untouchable", func(t *testing.T) {
		for _, name := range []string{"test", "other-admin"} {
			if rec := tenant.do("DELETE", "/api/v1/tokens/"+all[name], ""); rec.Code != http.StatusNotFound {
				t.Fatalf("%s: code = %d", name, rec.Code)
			}
		}
	})
}

func TestInstanceAdminIssuesTenantToken(t *testing.T) {
	_, tenant := tenantAdminEnv(t)
	rec := tenant.do("GET", "/api/v1/tokens", "")
	var items []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 1 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if items[0]["scope"] != "tenant" || items[0]["tenant"] != "acme" || items[0]["role"] != "admin" {
		t.Fatalf("token = %v", items[0])
	}
	if rec := tenant.do("GET", "/api/v1/tenants/acme/projects", ""); rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestTenantAdminCannotIssueForOtherTenant(t *testing.T) {
	_, tenant := tenantAdminEnv(t)
	rec := tenant.do("POST", "/api/v1/tokens", `{"name":"x","tenant":"other"}`)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := tenant.do("POST", "/api/v1/tokens", `{"name":"y"}`); rec.Code != http.StatusCreated || decode(t, rec)["tenant"] != "acme" {
		t.Fatal(rec.Code, rec.Body.String())
	}
}

func TestTenantTokenRejectedAfterTenantDeleted(t *testing.T) {
	instance, tenant := tenantAdminEnv(t)
	if rec := instance.do("DELETE", "/api/v1/tenants/acme", ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := tenant.do("GET", "/api/v1/tokens", ""); rec.Code != http.StatusUnauthorized {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if rec := instance.do("GET", "/api/v1/tenants", ""); rec.Code != http.StatusOK {
		t.Fatal("instance token must keep working:", rec.Code, rec.Body.String())
	}
}

func TestTenantAdminRenameKeepsAdapters(t *testing.T) {
	_, tenant := tenantAdminEnv(t)
	rec := tenant.do("PUT", "/api/v1/tenants/acme", `{"displayName":"Renamed"}`)
	got := decode(t, rec)
	if rec.Code != http.StatusOK || got["displayName"] != "Renamed" || got["trackerAdapter"] != "linear" || got["codeHostAdapter"] != "github" {
		t.Fatal(rec.Code, rec.Body.String())
	}
}
