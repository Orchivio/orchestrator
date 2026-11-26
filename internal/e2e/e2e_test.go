//go:build e2e

// Package e2e runs the server core in-process against fake adapters and a
// fake worker. Needs Docker/Podman for the Postgres testcontainer.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/worker/v1"
	"github.com/orchivio/orchestrator/internal/adapters"
	"github.com/orchivio/orchestrator/internal/admin"
	"github.com/orchivio/orchestrator/internal/config"
	"github.com/orchivio/orchestrator/internal/grpcapi"
	"github.com/orchivio/orchestrator/internal/httpapi"
	"github.com/orchivio/orchestrator/internal/orchestrator"
	"github.com/orchivio/orchestrator/internal/reporter"
	"github.com/orchivio/orchestrator/internal/router"
	"github.com/orchivio/orchestrator/internal/secretbox"
	"github.com/orchivio/orchestrator/internal/store"
)

const (
	userEmail     = "dev@example.com"
	workerID      = "w1"
	repoSlug      = "acme/playground"
	waitMax       = 5 * time.Second
	secretKey     = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	trackerCred   = "linear-token-ws1"
	codeHostCred  = "github-installation-42"
	repoToken     = "ghs_short_lived"
	providerLogin = "https://provider.test/authorize?state="
)

// errNoCredential is what the fakes answer to a call without the tenant's credential.
var errNoCredential = status.Error(codes.Unauthenticated, "missing or wrong credential")

func hasCred(c *pluginv1.Credential, want string) bool { return string(c.GetData()) == want }

func hasCreds(cs []*pluginv1.Credential, want string) bool {
	return len(cs) == 1 && hasCred(cs[0], want)
}

// connectFake implements the connection RPCs both fakes share: it hands out
// cred and allows the items in allowed.
type connectFake struct {
	cred, externalID string
	allowed          []string
}

func (f connectFake) BeginConnect(_ context.Context, r *pluginv1.BeginConnectRequest) (*pluginv1.BeginConnectResponse, error) {
	return &pluginv1.BeginConnectResponse{Url: providerLogin + url.QueryEscape(r.GetState())}, nil
}

func (f connectFake) CompleteConnect(_ context.Context, r *pluginv1.CompleteConnectRequest) (*pluginv1.CompleteConnectResponse, error) {
	if r.GetQuery()["code"] != "granted" {
		return nil, status.Error(codes.PermissionDenied, "no grant")
	}
	return &pluginv1.CompleteConnectResponse{Credential: &pluginv1.Credential{Data: []byte(f.cred)}, Label: "Acme " + f.externalID, ExternalId: f.externalID}, nil
}

func (f connectFake) CheckAccess(_ context.Context, r *pluginv1.CheckAccessRequest) (*pluginv1.CheckAccessResponse, error) {
	if !hasCreds(r.GetCredentials(), f.cred) {
		return nil, errNoCredential
	}
	var denied []string
	for _, it := range r.GetItems() {
		if !slices.Contains(f.allowed, it) {
			denied = append(denied, it)
		}
	}
	return &pluginv1.CheckAccessResponse{Denied: denied}, nil
}

// The Unimplemented servers sit one level deeper so connectFake's methods win.
type (
	trackerBase struct {
		pluginv1.UnimplementedTrackerAdapterServer
	}
	codeHostBase struct {
		pluginv1.UnimplementedCodeHostAdapterServer
	}
)

type fakeTracker struct {
	trackerBase
	connectFake
	mu       sync.Mutex
	streams  map[chan *pluginv1.TrackerEvent][]string // open streams -> watched project keys
	comments []string
}

// emit delivers ev to every open stream watching its project key and reports whether one did.
func (f *fakeTracker) emit(ev *pluginv1.TrackerEvent) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	delivered := false
	for ch, keys := range f.streams {
		if slices.Contains(keys, ev.GetProjectKey()) {
			select { // never block under the mutex; a full stream counts as not delivered and the caller retries
			case ch <- ev:
				delivered = true
			default:
			}
		}
	}
	return delivered
}

func (f *fakeTracker) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-tracker"}, nil
}

func (f *fakeTracker) StreamEvents(req *pluginv1.StreamEventsRequest, s grpc.ServerStreamingServer[pluginv1.TrackerEvent]) error {
	if !hasCred(req.GetCredential(), trackerCred) {
		return errNoCredential
	}
	events := make(chan *pluginv1.TrackerEvent, 8)
	f.mu.Lock()
	f.streams[events] = req.GetProjectKeys()
	f.mu.Unlock()
	defer func() { f.mu.Lock(); delete(f.streams, events); f.mu.Unlock() }()
	for {
		select {
		case <-s.Context().Done():
			return nil
		case ev := <-events:
			if err := s.Send(ev); err != nil {
				return err
			}
		}
	}
}

func (f *fakeTracker) GetTicket(_ context.Context, r *pluginv1.GetTicketRequest) (*pluginv1.Ticket, error) {
	if !hasCred(r.GetCredential(), trackerCred) {
		return nil, errNoCredential
	}
	return &pluginv1.Ticket{TicketRef: r.GetTicketRef(), Title: "Titel " + r.GetTicketRef()}, nil
}

func (f *fakeTracker) PostComment(_ context.Context, r *pluginv1.PostCommentRequest) (*pluginv1.PostCommentResponse, error) {
	if !hasCred(r.GetCredential(), trackerCred) {
		return nil, errNoCredential
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, r.GetTicketRef()+"|"+r.GetBody())
	return &pluginv1.PostCommentResponse{}, nil
}

func (f *fakeTracker) commentsFor(ref string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments {
		if strings.HasPrefix(c, ref+"|") {
			out = append(out, c)
		}
	}
	return out
}

type fakeCodeHost struct {
	codeHostBase
	connectFake
	mu  sync.Mutex
	prs []*pluginv1.OpenPullRequestRequest
}

func (f *fakeCodeHost) IssueRepoCredentials(_ context.Context, r *pluginv1.IssueRepoCredentialsRequest) (*pluginv1.RepoCredentials, error) {
	if !hasCreds(r.GetCredentials(), codeHostCred) {
		return nil, errNoCredential
	}
	return &pluginv1.RepoCredentials{Username: "x-access-token", Token: repoToken}, nil
}

func (f *fakeCodeHost) GetCapabilities(context.Context, *pluginv1.GetCapabilitiesRequest) (*pluginv1.Capabilities, error) {
	return &pluginv1.Capabilities{ContractVersion: "plugin.v1", AdapterName: "fake-codehost"}, nil
}

func (f *fakeCodeHost) OpenPullRequest(_ context.Context, r *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	if !hasCreds(r.GetCredentials(), codeHostCred) {
		return nil, errNoCredential
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, r)
	return &pluginv1.PullRequest{Url: "https://github.test/acme/playground/pull/7", Number: 7}, nil
}

func (f *fakeCodeHost) opened() []*pluginv1.OpenPullRequestRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pluginv1.OpenPullRequestRequest(nil), f.prs...)
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(waitMax)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

func serve(t *testing.T, register func(*grpc.Server)) *bufconn.Listener {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer()
	register(gs)
	go func() { _ = gs.Serve(lis) }()
	t.Cleanup(gs.Stop)
	return lis
}

func dial(t *testing.T, lis *bufconn.Listener) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func startPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("orchivio"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ctr.Terminate(ctx) })
	url, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return url
}

type env struct {
	tracker  *fakeTracker
	codeHost *fakeCodeHost
	db       *store.Store
	worker   workerv1.WorkerServiceClient
	admin    *httptest.Server
	token    string
	t        *testing.T
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := store.Open(ctx, startPostgres(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	ft := &fakeTracker{streams: map[chan *pluginv1.TrackerEvent][]string{},
		connectFake: connectFake{cred: trackerCred, externalID: "ws1", allowed: []string{"ORC", "RES"}}}
	fc := &fakeCodeHost{connectFake: connectFake{cred: codeHostCred, externalID: "42", allowed: []string{repoSlug}}}
	lisT := serve(t, func(s *grpc.Server) { pluginv1.RegisterTrackerAdapterServer(s, ft) })
	lisC := serve(t, func(s *grpc.Server) { pluginv1.RegisterCodeHostAdapterServer(s, fc) })
	dialer := func(addr string) (*grpc.ClientConn, error) {
		if addr == "tracker" {
			return dial(t, lisT), nil
		}
		return dial(t, lisC), nil
	}

	box, err := secretbox.New(secretKey)
	if err != nil {
		t.Fatal(err)
	}
	connReg := adapters.NewRegistry(nil, log)
	connReg.SetDialer(dialer)
	t.Cleanup(connReg.Close)
	addrs := map[string]string{"linear": "tracker", "github": "codehost"}
	svc := admin.NewService(db, addrs, box, connReg.Connector(addrs), "http://orchivio.test", time.Now, uuid.New, log)
	token := "orv_" + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.ReplaceAll(uuid.NewString(), "-", "")
	if ok, err := svc.Bootstrap(ctx, token); err != nil || !ok {
		t.Fatalf("bootstrap ok=%v err=%v", ok, err)
	}
	cfg, err := config.NewSync(ctx, svc, time.Hour, log)
	if err != nil {
		t.Fatal(err)
	}
	svc.OnChange(cfg.Reload)
	mux := http.NewServeMux() // as in main.go
	mux.Handle("/api/v1/", httpapi.NewAdmin(svc, log))
	mux.Handle("GET /oauth/callback/{adapter}", httpapi.NewCallback(svc, log))
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)

	reg := adapters.NewRegistry(cfg, log)
	t.Cleanup(reg.Close)
	reg.SetDialer(dialer)
	rep := reporter.New(func(tn string) reporter.TrackerClient { return reg.TrackerFor(tn) }, cfg)
	rt := router.New(cfg, db, reg.Tracker(), reg.CodeHost(), rep, time.Now, uuid.New)
	orch := orchestrator.New(cfg, db, reg.CodeHost(), reg.Tracker(), rep, rt.StartFollowUp, time.Now, time.Minute)
	lisW := serve(t, func(s *grpc.Server) { workerv1.RegisterWorkerServiceServer(s, grpcapi.NewWorker(orch)) })

	done := make(chan struct{})
	go func() { defer close(done); adapters.Run(ctx, cfg, reg, rt.Handle, log) }()
	t.Cleanup(func() { cancel(); <-done })

	return &env{tracker: ft, codeHost: fc, db: db, worker: workerv1.NewWorkerServiceClient(dial(t, lisW)), admin: api, token: token, t: t}
}

func (e *env) send(kind pluginv1.TrackerEvent_Kind, ref, body string, mention bool) {
	e.sendTo("ORC", kind, ref, body, mention)
}

// sendTo waits until a stream watches key, then delivers the event.
func (e *env) sendTo(key string, kind pluginv1.TrackerEvent_Kind, ref, body string, mention bool) {
	ev := &pluginv1.TrackerEvent{
		ExternalId: uuid.NewString(), Kind: kind, TicketRef: ref, ProjectKey: key,
		Actor: &pluginv1.Actor{Id: "u1", Email: userEmail}, Body: body, MentionsBot: mention,
	}
	eventually(e.t, "stream for project "+key, func() bool { return e.tracker.emit(ev) })
}

// post sends a JSON request to the admin API and decodes a 2xx response into out (if non-nil).
func (e *env) post(path string, in, out any) int {
	e.t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		e.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.admin.URL+path, bytes.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func (e *env) mustPost(path string, in, out any) {
	e.t.Helper()
	if code := e.post(path, in, out); code != http.StatusCreated {
		e.t.Fatalf("POST %s = %d, want 201", path, code)
	}
}

// connect authorizes tenant acme at adapter through the API and follows the
// provider's redirect back to the callback page.
func (e *env) connect(adapter string) {
	e.t.Helper()
	var auth struct {
		URL string `json:"url"`
	}
	if code := e.post("/api/v1/tenants/acme/connections/"+adapter+"/authorize", nil, &auth); code != http.StatusOK {
		e.t.Fatalf("authorize %s = %d", adapter, code)
	}
	state, ok := strings.CutPrefix(auth.URL, providerLogin)
	if !ok {
		e.t.Fatalf("authorize url = %q", auth.URL)
	}
	resp, err := http.Get(e.admin.URL + "/oauth/callback/" + adapter + "?code=granted&state=" + state)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("callback %s = %d", adapter, resp.StatusCode)
	}
}

// createTenant creates tenant acme with profile default and returns the profile id.
func (e *env) createTenant() string {
	e.t.Helper()
	var profile struct {
		ID string `json:"id"`
	}
	e.mustPost("/api/v1/tenants", map[string]any{"slug": "acme", "displayName": "Acme",
		"trackerAdapter": "linear", "codeHostAdapter": "github"}, nil)
	e.mustPost("/api/v1/tenants/acme/profiles", map[string]any{"name": "default", "runtime": "claude-code",
		"model": "m", "maxDuration": "20m"}, &profile)
	return profile.ID
}

func projectRequest(trackerKey, profileID string) map[string]any {
	return map[string]any{
		"name": "demo", "trackerKey": trackerKey, "profileId": profileID,
		"repos":        []config.Repo{{Slug: repoSlug, BaseBranch: "main", AllowedPaths: []string{""}}},
		"classifier":   config.ClassifierRules{TaskLabels: []string{"orchivio:aufgabe"}, QuestionLabels: []string{"orchivio:frage"}, DefaultRepo: repoSlug},
		"roles":        config.Roles{Questions: []string{userEmail}, Tasks: []string{userEmail}},
		"maxDiffLines": 500,
	}
}

// provision creates tenant acme (once per env), connects it to both fakes and creates a project for trackerKey.
func (e *env) provision(trackerKey string) {
	e.t.Helper()
	profileID := e.createTenant()
	e.connect("linear")
	e.connect("github")
	e.mustPost("/api/v1/tenants/acme/projects", projectRequest(trackerKey, profileID), nil)
}

func (e *env) lease(t *testing.T) *workerv1.RunAssignment {
	t.Helper()
	var a *workerv1.RunAssignment
	eventually(t, "queued run", func() bool {
		resp, err := e.worker.LeaseRun(context.Background(), &workerv1.LeaseRunRequest{WorkerId: workerID})
		if err != nil {
			t.Fatal(err)
		}
		a = resp.GetRun()
		return a != nil
	})
	return a
}

func (e *env) reportAndComplete(t *testing.T, a *workerv1.RunAssignment, req *workerv1.CompleteRunRequest) {
	t.Helper()
	ctx := context.Background()
	stream, err := e.worker.ReportEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"run.started", "message", "run.finished"} {
		if err := stream.Send(&workerv1.ReportEventsRequest{WorkerId: workerID, RunId: a.GetRunId(), Event: &pluginv1.AgentEvent{Type: typ, Text: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := stream.CloseAndRecv(); err != nil {
		t.Fatal(err)
	}
	req.RunId, req.WorkerId = a.GetRunId(), workerID
	if _, err := e.worker.CompleteRun(ctx, req); err != nil {
		t.Fatal(err)
	}
}

// get sends an authenticated GET to the admin API and decodes a 200 response into out.
func (e *env) get(t *testing.T, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.admin.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
}

// assertRunReadable fetches the run named in the ticket comment through the runs API.
func (e *env) assertRunReadable(t *testing.T, runID string) {
	t.Helper()
	var projects []struct {
		ID string `json:"id"`
	}
	e.get(t, "/api/v1/tenants/acme/projects", &projects)
	if len(projects) != 1 {
		t.Fatalf("projects = %v, want exactly one", projects)
	}
	base := "/api/v1/tenants/acme/projects/" + projects[0].ID + "/runs"
	var one struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		Events []json.RawMessage `json:"events"`
	}
	e.get(t, base+"/"+runID, &one)
	if one.Run.ID != runID || len(one.Events) == 0 {
		t.Fatalf("run %q with %d events, want id %s and at least one event", one.Run.ID, len(one.Events), runID)
	}
	var list struct {
		Runs []struct {
			ID string `json:"id"`
		} `json:"runs"`
	}
	e.get(t, base, &list)
	for _, r := range list.Runs {
		if r.ID == runID {
			return
		}
	}
	t.Fatalf("run %s missing from %v", runID, list.Runs)
}

func hasComment(e *env, ref, substr string) func() bool {
	return func() bool {
		for _, c := range e.tracker.commentsFor(ref) {
			if strings.Contains(c, substr) {
				return true
			}
		}
		return false
	}
}

func TestServerStartsEmptyAndAcceptsBootstrapToken(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest(http.MethodGet, e.admin.URL+"/api/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != http.StatusOK || len(got) != 0 {
		t.Fatalf("status=%d tenants=%v err=%v, want 200 []", resp.StatusCode, got, err)
	}
}

func TestConnectFlowThenProjectReceivesEvents(t *testing.T) {
	e := setup(t)
	profileID := e.createTenant()
	if code := e.post("/api/v1/tenants/acme/projects", projectRequest("RES", profileID), nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("project before connecting = %d, want 422", code)
	}
	e.connect("linear")
	e.connect("github")
	denied := projectRequest("RES", profileID)
	denied["repos"] = []config.Repo{{Slug: "other/repo", BaseBranch: "main"}}
	denied["classifier"] = config.ClassifierRules{DefaultRepo: "other/repo"}
	if code := e.post("/api/v1/tenants/acme/projects", denied, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("project with foreign repo = %d, want 422", code)
	}
	e.mustPost("/api/v1/tenants/acme/projects", projectRequest("RES", profileID), nil)

	e.sendTo("RES", pluginv1.TrackerEvent_ASSIGNED, "RES-1", "", false)
	a := e.lease(t)
	creds, err := e.worker.GetRepoCredentials(context.Background(),
		&workerv1.GetRepoCredentialsRequest{WorkerId: workerID, RunId: a.GetRunId(), Write: true})
	if err != nil || creds.GetToken() != repoToken {
		t.Fatalf("repo credentials = %v, err %v", creds, err)
	}
}

func TestProjectAddedAtRuntimeReceivesEvents(t *testing.T) {
	e := setup(t)
	e.provision("RES")
	e.sendTo("RES", pluginv1.TrackerEvent_ASSIGNED, "RES-1", "", false)
	a := e.lease(t)
	if a.GetKind() != "task" || !strings.Contains(a.GetBranch(), "res-1") {
		t.Fatalf("kind=%q branch=%q, want task on res-1", a.GetKind(), a.GetBranch())
	}
}

func TestEndToEndWithFakes(t *testing.T) {
	e := setup(t)
	e.provision("ORC")

	t.Run("question gets an answer comment with run id", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-1", "@Orchivio Was steht in der README?", true)
		a := e.lease(t)
		if a.GetKind() != "question" {
			t.Fatalf("kind = %q, want question", a.GetKind())
		}
		e.reportAndComplete(t, a, &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: "Die README ist leer."})
		eventually(t, "answer comment", hasComment(e, "ORC-1", "Die README ist leer."))
		eventually(t, "run id", hasComment(e, "ORC-1", "Run-ID: `"+a.GetRunId()+"`"))
		e.assertRunReadable(t, a.GetRunId())
	})

	t.Run("task opens a pull request and links it in the ticket", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_ASSIGNED, "ORC-2", "", false)
		a := e.lease(t)
		wantBranch := "orchivio/orc-2-" + a.GetRunId()[:8] + "-titel-orc-2"
		if a.GetKind() != "task" || a.GetBranch() != wantBranch {
			t.Fatalf("kind=%q branch=%q, want task %q", a.GetKind(), a.GetBranch(), wantBranch)
		}
		e.reportAndComplete(t, a, &workerv1.CompleteRunRequest{
			Outcome: workerv1.CompleteRunRequest_PUSHED, Text: "Kontakt ergänzt.",
			PushedBranches: map[string]string{repoSlug: a.GetBranch()},
		})
		eventually(t, "PR comment", hasComment(e, "ORC-2", "https://github.test/acme/playground/pull/7"))
		prs := e.codeHost.opened()
		if len(prs) != 1 || prs[0].GetRepo() != repoSlug || prs[0].GetHead() != wantBranch || prs[0].GetBase() != "main" {
			t.Fatalf("OpenPullRequest calls = %v", prs)
		}
	})

	t.Run("comment during running run starts a follow-up run after completion", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-3", "@Orchivio Erste Frage", true)
		first := e.lease(t)
		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-3", "Und noch eine Frage", false)
		eventually(t, "open input", func() bool {
			conv, err := e.db.FindConversation(context.Background(), "acme", "ORC-3")
			if err != nil || conv == nil {
				return false
			}
			in, err := e.db.OpenInputs(context.Background(), conv.ID)
			return err == nil && len(in) == 1
		})
		resp, err := e.worker.LeaseRun(context.Background(), &workerv1.LeaseRunRequest{WorkerId: workerID})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetRun() != nil {
			t.Fatalf("second run leased while first still running: %s", resp.GetRun().GetRunId())
		}
		e.reportAndComplete(t, first, &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_ANSWERED, Text: "Antwort eins."})
		second := e.lease(t)
		if second.GetRunId() == first.GetRunId() || !strings.Contains(second.GetSpec().GetPrompt(), "Und noch eine Frage") {
			t.Fatalf("follow-up run %s prompt lacks second comment", second.GetRunId())
		}
	})
	t.Run("follow-up task pushes to the same branch and reuses the open pull request", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_ASSIGNED, "ORC-4", "", false)
		first := e.lease(t)
		e.reportAndComplete(t, first, &workerv1.CompleteRunRequest{
			Outcome: workerv1.CompleteRunRequest_PUSHED, Text: "Teil eins.",
			PushedBranches: map[string]string{repoSlug: first.GetBranch()},
		})
		eventually(t, "first run succeeded", runState(e, first, router.StateSucceeded))

		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-4", "Bitte noch Teil zwei", false)
		second := e.lease(t)
		if second.GetRunId() == first.GetRunId() || second.GetKind() != "task" || second.GetBranch() != first.GetBranch() {
			t.Fatalf("follow-up run %s kind=%q branch=%q, want task on %q", second.GetRunId(), second.GetKind(), second.GetBranch(), first.GetBranch())
		}
		e.reportAndComplete(t, second, &workerv1.CompleteRunRequest{
			Outcome: workerv1.CompleteRunRequest_PUSHED, Text: "Teil zwei.",
			PushedBranches: map[string]string{repoSlug: second.GetBranch()},
		})
		eventually(t, "follow-up run succeeded", runState(e, second, router.StateSucceeded))
		var heads []string
		for _, pr := range e.codeHost.opened() {
			if pr.GetHead() == first.GetBranch() {
				heads = append(heads, pr.GetHead())
			}
		}
		if len(heads) != 2 {
			t.Fatalf("OpenPullRequest on %s called %d times, want 2", first.GetBranch(), len(heads))
		}
	})

	t.Run("ask_human question precedes the answer in the resumed prompt", func(t *testing.T) {
		e.send(pluginv1.TrackerEvent_ASSIGNED, "ORC-5", "", false)
		a := e.lease(t)
		e.reportAndComplete(t, a, &workerv1.CompleteRunRequest{Outcome: workerv1.CompleteRunRequest_WAITING, Text: "Welche Farbe?"})
		eventually(t, "question comment", hasComment(e, "ORC-5", "Welche Farbe?"))

		e.send(pluginv1.TrackerEvent_COMMENTED, "ORC-5", "Blau, bitte.", false)
		resumed := e.lease(t)
		prompt := resumed.GetSpec().GetPrompt()
		q, ans := strings.Index(prompt, "Welche Farbe?"), strings.Index(prompt, "Blau, bitte.")
		if resumed.GetRunId() != a.GetRunId() || resumed.GetBranch() != a.GetBranch() || q < 0 || ans < 0 || q > ans {
			t.Fatalf("resumed run %s branch %q (want %s %q), prompt:\n%s", resumed.GetRunId(), resumed.GetBranch(), a.GetRunId(), a.GetBranch(), prompt)
		}
		if strings.Count(prompt, "Welche Farbe?") != 1 || !strings.Contains(prompt, "**Orchivio**") {
			t.Fatalf("question must appear once, authored by Orchivio:\n%s", prompt)
		}
	})
}

func runState(e *env, a *workerv1.RunAssignment, want string) func() bool {
	return func() bool {
		r, err := e.db.GetRun(context.Background(), uuid.MustParse(a.GetRunId()))
		return err == nil && r != nil && r.State == want
	}
}
