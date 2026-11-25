package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	adminPrefix  = "/api/v1"
	maxBodyBytes = 1 << 20
)

// Route is one admin API operation; Path is relative to /api/v1.
type Route struct{ Method, Path string }

// AdminRoutes is the route table; api/openapi.yaml must describe exactly these.
var AdminRoutes = []Route{
	{"GET", "/tenants"}, {"POST", "/tenants"},
	{"GET", "/tenants/{slug}"}, {"PUT", "/tenants/{slug}"}, {"DELETE", "/tenants/{slug}"},
	{"GET", "/tenants/{slug}/profiles"}, {"POST", "/tenants/{slug}/profiles"},
	{"GET", "/tenants/{slug}/profiles/{id}"}, {"PUT", "/tenants/{slug}/profiles/{id}"}, {"DELETE", "/tenants/{slug}/profiles/{id}"},
	{"GET", "/tenants/{slug}/projects"}, {"POST", "/tenants/{slug}/projects"},
	{"GET", "/tenants/{slug}/projects/{id}"}, {"PUT", "/tenants/{slug}/projects/{id}"}, {"DELETE", "/tenants/{slug}/projects/{id}"},
	{"GET", "/tenants/{slug}/connections"}, {"POST", "/tenants/{slug}/connections/{adapter}/authorize"},
	{"GET", "/tenants/{slug}/connections/{id}"}, {"DELETE", "/tenants/{slug}/connections/{id}"},
	{"GET", "/tokens"}, {"POST", "/tokens"}, {"DELETE", "/tokens/{id}"},
}

type adminAPI struct {
	svc *admin.Service
	log *slog.Logger
}

// NewAdmin serves the admin REST API under /api/v1, guarded by Authenticate.
// Every /tenants/{slug} route first checks that the principal sees that tenant.
func NewAdmin(svc *admin.Service, log *slog.Logger) http.Handler {
	a := &adminAPI{svc: svc, log: log}
	handlers := map[string]http.HandlerFunc{
		"GET /tenants": a.listTenants, "POST /tenants": a.createTenant,
		"GET /tenants/{slug}": a.getTenant, "PUT /tenants/{slug}": a.putTenant, "DELETE /tenants/{slug}": a.deleteTenant,
		"GET /tenants/{slug}/profiles": a.listProfiles, "POST /tenants/{slug}/profiles": a.createProfile,
		"GET /tenants/{slug}/profiles/{id}": a.getProfile, "PUT /tenants/{slug}/profiles/{id}": a.putProfile,
		"DELETE /tenants/{slug}/profiles/{id}": a.deleteProfile,
		"GET /tenants/{slug}/projects":         a.listProjects, "POST /tenants/{slug}/projects": a.createProject,
		"GET /tenants/{slug}/projects/{id}": a.getProject, "PUT /tenants/{slug}/projects/{id}": a.putProject,
		"DELETE /tenants/{slug}/projects/{id}":                 a.deleteProject,
		"GET /tenants/{slug}/connections":                      a.listConnections,
		"POST /tenants/{slug}/connections/{adapter}/authorize": a.authorize,
		"GET /tenants/{slug}/connections/{id}":                 a.getConnection,
		"DELETE /tenants/{slug}/connections/{id}":              a.deleteConnection,
		"GET /tokens": a.listTokens, "POST /tokens": a.createToken, "DELETE /tokens/{id}": a.revokeToken,
	}
	mux := http.NewServeMux()
	for _, r := range AdminRoutes {
		h, ok := handlers[r.Method+" "+r.Path]
		if !ok {
			panic("httpapi: no handler for " + r.Method + " " + r.Path)
		}
		if strings.Contains(r.Path, "{slug}") {
			h = a.tenantScoped(h)
		}
		mux.Handle(r.Method+" "+adminPrefix+r.Path, h)
	}
	return Authenticate(svc, mux)
}

// tenantScoped answers 404 before h runs when the principal may not see the path's tenant.
func (a *adminAPI) tenantScoped(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := principal(r).SeesTenant(r.PathValue("slug")); err != nil {
			a.fail(w, err)
			return
		}
		h(w, r)
	}
}

// principal is the caller; Authenticate guarantees one. The zero value is
// ErrForbidden in every admin.Service ...For function and in SeesTenant.
func principal(r *http.Request) admin.Principal {
	p, _ := PrincipalFrom(r.Context())
	return p
}

// --- DTOs (server-managed fields are accepted and ignored on input) ---

type tenantDTO struct {
	Slug            string    `json:"slug"`
	DisplayName     string    `json:"displayName"`
	TrackerAdapter  string    `json:"trackerAdapter"`
	CodeHostAdapter string    `json:"codeHostAdapter"`
	Version         int64     `json:"version"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

type profileDTO struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Runtime      string    `json:"runtime"`
	Model        string    `json:"model"`
	Instructions string    `json:"instructions"`
	MaxDuration  string    `json:"maxDuration"`
	Version      int64     `json:"version"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type projectDTO struct {
	ID           string                 `json:"id"`
	Name         string                 `json:"name"`
	TrackerKey   string                 `json:"trackerKey"`
	ProfileID    string                 `json:"profileId"`
	Repos        []config.Repo          `json:"repos"`
	Classifier   config.ClassifierRules `json:"classifier"`
	Roles        config.Roles           `json:"roles"`
	MaxDiffLines int                    `json:"maxDiffLines"`
	Milestones   []string               `json:"milestones"`
	Version      int64                  `json:"version"`
	CreatedAt    time.Time              `json:"createdAt"`
	UpdatedAt    time.Time              `json:"updatedAt"`
}

type tokenDTO struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Prefix    string     `json:"prefix"`
	Scope     string     `json:"scope"`
	Tenant    string     `json:"tenant,omitempty"`
	Role      string     `json:"role"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt"`
}

type tokenCreatedDTO struct {
	tokenDTO
	Token string `json:"token"`
}

type tokenRequest struct {
	Name      string `json:"name"`
	ExpiresIn string `json:"expiresIn"`
	Tenant    string `json:"tenant"`
}

func tenantOut(t admin.Tenant) tenantDTO {
	return tenantDTO{t.Slug, t.DisplayName, t.TrackerAdapter, t.CodeHostAdapter, t.Version, t.CreatedAt, t.UpdatedAt}
}

func profileOut(p admin.Profile) profileDTO {
	return profileDTO{p.ID.String(), p.Name, p.Runtime, p.Model, p.Instructions, p.MaxDuration.String(), p.Version, p.CreatedAt, p.UpdatedAt}
}

func projectOut(p admin.Project) projectDTO {
	return projectDTO{p.ID.String(), p.Name, p.TrackerKey, p.ProfileID.String(), reposOut(p.Repos), classifierOut(p.Classifier), rolesOut(p.Roles),
		p.MaxDiffLines, nonNil(p.Milestones), p.Version, p.CreatedAt, p.UpdatedAt}
}

func tokenOut(t admin.Token) tokenDTO {
	return tokenDTO{t.ID.String(), t.Name, t.Prefix, t.Scope, t.TenantSlug, t.Role, t.CreatedAt, t.ExpiresAt, t.RevokedAt}
}

func reposOut(repos []config.Repo) []config.Repo {
	out := make([]config.Repo, len(repos))
	for i, r := range repos {
		r.AllowedPaths, r.Users = nonNil(r.AllowedPaths), nonNil(r.Users)
		out[i] = r
	}
	return out
}

func classifierOut(c config.ClassifierRules) config.ClassifierRules {
	c.TaskLabels, c.QuestionLabels = nonNil(c.TaskLabels), nonNil(c.QuestionLabels)
	if c.RepoLabels == nil {
		c.RepoLabels = map[string]string{}
	}
	return c
}

func rolesOut(r config.Roles) config.Roles {
	r.Questions, r.Tasks = nonNil(r.Questions), nonNil(r.Tasks)
	return r
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func fieldProblem(field, message string) error {
	return &admin.ValidationError{Fields: []config.FieldError{{Field: field, Message: message}}}
}

func (d profileDTO) domain(tenant string, id uuid.UUID) (admin.Profile, error) {
	dur, err := time.ParseDuration(d.MaxDuration)
	if err != nil {
		return admin.Profile{}, fieldProblem("maxDuration", `must be a duration such as "20m"`)
	}
	return admin.Profile{ID: id, TenantSlug: tenant, Name: d.Name, Runtime: d.Runtime, Model: d.Model,
		Instructions: d.Instructions, MaxDuration: dur}, nil
}

func (d projectDTO) domain(tenant string, id uuid.UUID) (admin.Project, error) {
	profileID, err := uuid.Parse(d.ProfileID)
	if err != nil {
		return admin.Project{}, fieldProblem("profileId", "must be a UUID")
	}
	return admin.Project{ID: id, TenantSlug: tenant, Name: d.Name, TrackerKey: d.TrackerKey, ProfileID: profileID,
		Repos: d.Repos, Classifier: d.Classifier, Roles: d.Roles, MaxDiffLines: d.MaxDiffLines, Milestones: d.Milestones}, nil
}

// --- plumbing ---

func (a *adminAPI) send(w http.ResponseWriter, status int, version int64, body any) {
	w.Header().Set("Content-Type", "application/json")
	if version > 0 {
		w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(version, 10)))
	}
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		a.log.Warn("write response failed", "err", err)
	}
}

func (a *adminAPI) created(w http.ResponseWriter, location string, version int64, body any) {
	w.Header().Set("Location", adminPrefix+location)
	a.send(w, http.StatusCreated, version, body)
}

// decodeBody reads a strict JSON body; on failure it has already answered 400.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "The request body is not valid JSON for this resource.")
		return false
	}
	return true
}

// ifMatch parses If-Match; absent means 0 (no precondition).
func ifMatch(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.Trim(r.Header.Get("If-Match"), `"`)
	if raw == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		writeProblem(w, http.StatusBadRequest, "Bad Request", "If-Match must be a quoted version number.")
		return 0, false
	}
	return v, true
}

// pathID parses the {id} path value; an unparsable id cannot exist, so 404.
func pathID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, http.StatusNotFound, "Not Found", "The resource does not exist.")
		return uuid.Nil, false
	}
	return id, true
}

func (a *adminAPI) fail(w http.ResponseWriter, err error) {
	var ve *admin.ValidationError
	switch {
	case errors.As(err, &ve):
		writeProblemFields(w, http.StatusUnprocessableEntity, "Validation Failed", "One or more fields are invalid.", ve.Fields)
	case errors.Is(err, admin.ErrForbidden):
		writeProblem(w, http.StatusForbidden, "Forbidden", "This token may not perform this action.")
	case errors.Is(err, admin.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "Not Found", "The resource does not exist.")
	case errors.Is(err, admin.ErrInUse):
		writeProblem(w, http.StatusConflict, "Conflict", "The resource is still in use.")
	case errors.Is(err, admin.ErrConflict):
		writeProblem(w, http.StatusConflict, "Conflict", "Already connected to another tenant.")
	case errors.Is(err, admin.ErrUnavailable):
		a.log.Warn("connections unavailable", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "Service Unavailable", "Connections are not available right now.")
	case errors.Is(err, admin.ErrAdapter), errors.Is(err, admin.ErrCredentialRejected), errors.Is(err, admin.ErrNotConnected):
		a.log.Warn("adapter request failed", "err", err)
		writeProblem(w, http.StatusBadGateway, "Bad Gateway", "The adapter could not handle the request.")
	case errors.Is(err, admin.ErrVersionMismatch):
		writeProblem(w, http.StatusPreconditionFailed, "Precondition Failed", "The resource was changed; reload and retry.")
	default:
		a.log.Error("admin request failed", "err", err)
		writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "The request could not be processed.")
	}
}

func list[T, D any](a *adminAPI, w http.ResponseWriter, items []T, err error, out func(T) D) {
	if err != nil {
		a.fail(w, err)
		return
	}
	dtos := make([]D, len(items))
	for i, it := range items {
		dtos[i] = out(it)
	}
	a.send(w, http.StatusOK, 0, dtos)
}

// ponytail: no pagination, add when a tenant has hundreds of projects

// --- tenants ---

func (a *adminAPI) listTenants(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListTenantsFor(r.Context(), principal(r))
	list(a, w, items, err, tenantOut)
}

func (a *adminAPI) createTenant(w http.ResponseWriter, r *http.Request) {
	var d tenantDTO
	if !decodeBody(w, r, &d) {
		return
	}
	t, err := a.svc.CreateTenantFor(r.Context(), principal(r), admin.Tenant{Slug: d.Slug, DisplayName: d.DisplayName,
		TrackerAdapter: d.TrackerAdapter, CodeHostAdapter: d.CodeHostAdapter})
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/tenants/"+t.Slug, t.Version, tenantOut(t))
}

func (a *adminAPI) getTenant(w http.ResponseWriter, r *http.Request) {
	t, err := a.svc.GetTenant(r.Context(), r.PathValue("slug"))
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, t.Version, tenantOut(t))
}

func (a *adminAPI) putTenant(w http.ResponseWriter, r *http.Request) {
	var d tenantDTO
	version, ok := ifMatch(w, r)
	if !ok || !decodeBody(w, r, &d) {
		return
	}
	t, err := a.svc.UpdateTenantFor(r.Context(), principal(r), admin.Tenant{Slug: r.PathValue("slug"), DisplayName: d.DisplayName,
		TrackerAdapter: d.TrackerAdapter, CodeHostAdapter: d.CodeHostAdapter}, version)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, t.Version, tenantOut(t))
}

func (a *adminAPI) deleteTenant(w http.ResponseWriter, r *http.Request) {
	version, ok := ifMatch(w, r)
	if ok {
		a.noContent(w, a.svc.DeleteTenantFor(r.Context(), principal(r), r.PathValue("slug"), version))
	}
}

func (a *adminAPI) noContent(w http.ResponseWriter, err error) {
	if err != nil {
		a.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- profiles ---

func (a *adminAPI) listProfiles(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListProfiles(r.Context(), r.PathValue("slug"))
	list(a, w, items, err, profileOut)
}

func (a *adminAPI) createProfile(w http.ResponseWriter, r *http.Request) {
	var d profileDTO
	if !decodeBody(w, r, &d) {
		return
	}
	slug := r.PathValue("slug")
	p, err := d.domain(slug, uuid.Nil)
	if err == nil {
		p, err = a.svc.CreateProfile(r.Context(), p)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/tenants/"+slug+"/profiles/"+p.ID.String(), p.Version, profileOut(p))
}

func (a *adminAPI) getProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.svc.GetProfile(r.Context(), r.PathValue("slug"), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, p.Version, profileOut(p))
}

func (a *adminAPI) putProfile(w http.ResponseWriter, r *http.Request) {
	var d profileDTO
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	version, ok := ifMatch(w, r)
	if !ok || !decodeBody(w, r, &d) {
		return
	}
	p, err := d.domain(r.PathValue("slug"), id)
	if err == nil {
		p, err = a.svc.UpdateProfile(r.Context(), p, version)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, p.Version, profileOut(p))
}

func (a *adminAPI) deleteProfile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if version, ok := ifMatch(w, r); ok {
		a.noContent(w, a.svc.DeleteProfile(r.Context(), r.PathValue("slug"), id, version))
	}
}

// --- projects ---

func (a *adminAPI) listProjects(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListProjects(r.Context(), r.PathValue("slug"))
	list(a, w, items, err, projectOut)
}

func (a *adminAPI) createProject(w http.ResponseWriter, r *http.Request) {
	var d projectDTO
	if !decodeBody(w, r, &d) {
		return
	}
	slug := r.PathValue("slug")
	p, err := d.domain(slug, uuid.Nil)
	if err == nil {
		p, err = a.svc.CreateProject(r.Context(), p)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.created(w, "/tenants/"+slug+"/projects/"+p.ID.String(), p.Version, projectOut(p))
}

func (a *adminAPI) getProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p, err := a.svc.GetProject(r.Context(), r.PathValue("slug"), id)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, p.Version, projectOut(p))
}

func (a *adminAPI) putProject(w http.ResponseWriter, r *http.Request) {
	var d projectDTO
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	version, ok := ifMatch(w, r)
	if !ok || !decodeBody(w, r, &d) {
		return
	}
	p, err := d.domain(r.PathValue("slug"), id)
	if err == nil {
		p, err = a.svc.UpdateProject(r.Context(), p, version)
	}
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusOK, p.Version, projectOut(p))
}

func (a *adminAPI) deleteProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if version, ok := ifMatch(w, r); ok {
		a.noContent(w, a.svc.DeleteProject(r.Context(), r.PathValue("slug"), id, version))
	}
}

// --- tokens ---

func (a *adminAPI) listTokens(w http.ResponseWriter, r *http.Request) {
	items, err := a.svc.ListTokensFor(r.Context(), principal(r))
	list(a, w, items, err, tokenOut)
}

func (a *adminAPI) createToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var ttl time.Duration
	if req.ExpiresIn != "" {
		var err error
		if ttl, err = time.ParseDuration(req.ExpiresIn); err != nil || ttl <= 0 {
			a.fail(w, fieldProblem("expiresIn", `must be a positive duration such as "720h"`))
			return
		}
	}
	tok, plaintext, err := a.svc.IssueTokenFor(r.Context(), principal(r), req.Name, ttl, req.Tenant)
	if err != nil {
		a.fail(w, err)
		return
	}
	a.send(w, http.StatusCreated, 0, tokenCreatedDTO{tokenOut(tok), plaintext})
}

func (a *adminAPI) revokeToken(w http.ResponseWriter, r *http.Request) {
	if id, ok := pathID(w, r); ok {
		a.noContent(w, a.svc.RevokeTokenFor(r.Context(), principal(r), id))
	}
}
