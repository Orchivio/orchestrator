package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/router"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
)

const branch = "orchivio/orc-12-abcd"

const cfgYAML = `tenant: acme
tracker: { name: linear, address: localhost:7101 }
codeHost: { name: github, address: localhost:7201 }
profiles:
  default: { runtime: claude-code, model: m1, instructions: be nice, maxDuration: 20m }
projects:
  - name: demo
    trackerKey: ORC
    profile: default
    repos:
      - { slug: soemii/a, baseBranch: main, allowedPaths: ["src/", "docs/"] }
      - { slug: soemii/b, baseBranch: develop, allowedPaths: ["src/"] }
    maxDiffLines: 300
`

type fakeStore struct {
	mu         sync.Mutex
	runs       map[uuid.UUID]*router.Run
	queue      []uuid.UUID
	events     []event
	inputs     []router.Input
	failUpdate bool
}

func (f *fakeStore) AddInput(_ context.Context, in router.Input) error {
	f.inputs = append(f.inputs, in)
	return nil
}

type event struct {
	typ     string
	payload any
}

func (f *fakeStore) LeaseNext(_ context.Context, w string, until time.Time) (*router.Run, error) {
	if len(f.queue) == 0 {
		return nil, nil
	}
	r := f.runs[f.queue[0]]
	f.queue = f.queue[1:]
	r.State, r.LeaseOwner, r.LeaseUntil = router.StateRunning, w, &until
	c := *r
	return &c, nil
}
func (f *fakeStore) GetRun(_ context.Context, id uuid.UUID) (*router.Run, error) {
	if r, ok := f.runs[id]; ok {
		c := *r
		return &c, nil
	}
	return nil, nil
}
func (f *fakeStore) UpdateRun(_ context.Context, r router.Run) error {
	if f.failUpdate {
		return errors.New("db down")
	}
	c := r
	f.runs[r.ID] = &c
	return nil
}
func (f *fakeStore) AppendEvent(_ context.Context, _ string, _ uuid.UUID, typ string, p any) error {
	f.events = append(f.events, event{typ, p})
	return nil
}

type fakeHost struct {
	credCalls int
	prs       []*pluginv1.OpenPullRequestRequest
	prErr     error
	failRepo  string
}

func (h *fakeHost) IssueRepoCredentials(context.Context, string, []string, bool) (*pluginv1.RepoCredentials, error) {
	h.credCalls++
	return &pluginv1.RepoCredentials{Token: "t"}, nil
}
func (h *fakeHost) OpenPullRequest(_ context.Context, _ string, req *pluginv1.OpenPullRequestRequest) (*pluginv1.PullRequest, error) {
	if h.prErr != nil {
		return nil, h.prErr
	}
	if req.Repo == h.failRepo {
		return nil, errors.New("422 boom")
	}
	h.prs = append(h.prs, req)
	return &pluginv1.PullRequest{Url: "https://gh/" + req.Repo + "/pull/1"}, nil
}

type fakeTracker struct{}

func (fakeTracker) GetTicket(context.Context, string, string) (*pluginv1.Ticket, error) {
	return &pluginv1.Ticket{Title: "Fix it"}, nil
}

type fakeReporter struct{ milestones []string }

func (f *fakeReporter) Milestone(_ context.Context, _ router.Run, m, text string) error {
	f.milestones = append(f.milestones, m+": "+text)
	return nil
}
func (f *fakeReporter) Notice(context.Context, string, string, string) error { return nil }

type fixture struct {
	o        *Orchestrator
	s        *fakeStore
	host     *fakeHost
	rep      *fakeReporter
	followed []uuid.UUID
	run      router.Run
	now      time.Time
}

func newFixture(t *testing.T, kind string) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	sync, err := config.NewSync(dir, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: &fakeStore{runs: map[uuid.UUID]*router.Run{}}, host: &fakeHost{}, rep: &fakeReporter{},
		now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	f.run = router.Run{ID: uuid.New(), Tenant: "acme", ConversationID: uuid.New(), Project: "ORC", TicketRef: "ORC-12",
		Kind: kind, State: router.StateQueued, Repos: []string{"soemii/a", "soemii/b"}, Branch: branch, Prompt: "do it"}
	r := f.run
	f.s.runs[r.ID] = &r
	f.s.queue = []uuid.UUID{r.ID}
	f.o = New(sync, f.s, f.host, fakeTracker{}, f.rep,
		func(_ context.Context, id uuid.UUID) error { f.followed = append(f.followed, id); return nil },
		func() time.Time { return f.now }, time.Hour, "https://orc.example/")
	return f
}

func (f *fixture) lease(t *testing.T) {
	t.Helper()
	if _, err := f.o.Lease(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) complete(w string, o workerv1.CompleteRunRequest_Outcome, text string, pushed map[string]string) error {
	return f.o.Complete(context.Background(), &workerv1.CompleteRunRequest{RunId: f.run.ID.String(), WorkerId: w,
		Outcome: o, Text: text, PushedBranches: pushed, SessionId: "sess-1"})
}

func (f *fixture) state() string { return f.s.runs[f.run.ID].State }

func TestLeaseBuildsAssignment(t *testing.T) {
	f := newFixture(t, "task")
	a, err := f.o.Lease(context.Background(), "w1")
	if err != nil || a == nil {
		t.Fatalf("Lease = %v, %v", a, err)
	}
	if a.RunId != f.run.ID.String() || a.TicketTitle != "Fix it" || a.TicketRef != "ORC-12" || a.Spec.Model != "m1" ||
		a.Spec.Instructions != "be nice" || a.Spec.Prompt != "do it" || a.MaxDiffLines != 300 || a.Kind != "task" {
		t.Errorf("assignment = %v", a)
	}
	if len(a.Repos) != 2 || a.Repos[1].BaseBranch != "develop" {
		t.Errorf("repos = %v", a.Repos)
	}
	if got := strings.Join(a.Repos[0].AllowedPaths, ","); got != "src/,docs/" {
		t.Errorf("allowed paths of soemii/a = %s", got)
	}
	if got := strings.Join(a.Repos[1].AllowedPaths, ","); got != "src/" { // no union: docs/ is not allowed in soemii/b
		t.Errorf("allowed paths of soemii/b = %s", got)
	}
	want := f.now.Add(25 * time.Minute)
	if !a.LeaseUntil.AsTime().Equal(want) || !f.s.runs[f.run.ID].LeaseUntil.Equal(want) {
		t.Errorf("lease until = %v, want %v", a.LeaseUntil.AsTime(), want)
	}
	if len(f.s.events) != 1 || f.s.events[0].typ != "run.state" {
		t.Errorf("events = %v", f.s.events)
	}
}

func TestLeaseEmptyQueueReturnsNil(t *testing.T) {
	f := newFixture(t, "task")
	f.s.queue = nil
	if a, err := f.o.Lease(context.Background(), "w1"); a != nil || err != nil {
		t.Fatalf("Lease = %v, %v", a, err)
	}
}

func TestCompleteRejectsForeignWorker(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	err := f.complete("intruder", workerv1.CompleteRunRequest_ANSWERED, "x", nil)
	if !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("err = %v", err)
	}
	if f.state() != router.StateRunning || len(f.rep.milestones) != 0 || len(f.followed) != 0 {
		t.Errorf("run changed: state=%s", f.state())
	}
}

func TestCompleteRejectsFinishedRun(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	_ = f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "x", nil)
	if err := f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "x", nil); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteUnspecifiedOutcomeRejected(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_OUTCOME_UNSPECIFIED, "", nil); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("err = %v", err)
	}
	if f.state() != router.StateRunning {
		t.Errorf("state = %s", f.state())
	}
}

func TestCredentialsWriteDeniedForQuestion(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	if _, err := f.o.Credentials(context.Background(), "w1", f.run.ID, true); !errors.Is(err, ErrWriteNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	if f.host.credCalls != 0 {
		t.Error("code host was called")
	}
	if _, err := f.o.Credentials(context.Background(), "w1", f.run.ID, false); err != nil || f.host.credCalls != 1 {
		t.Errorf("read credentials: %v, calls=%d", err, f.host.credCalls)
	}
}

func TestCredentialsForeignWorkerDenied(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	if _, err := f.o.Credentials(context.Background(), "x", f.run.ID, true); !errors.Is(err, ErrNotLeaseOwner) || f.host.credCalls != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteAnsweredPostsResult(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "42", nil); err != nil {
		t.Fatal(err)
	}
	r := f.s.runs[f.run.ID]
	if r.State != router.StateSucceeded || r.Result != "42" || r.SessionID != "sess-1" {
		t.Errorf("run = %+v", r)
	}
	if len(f.rep.milestones) != 1 || f.rep.milestones[0] != "result: 42" {
		t.Errorf("milestones = %v", f.rep.milestones)
	}
	if len(f.followed) != 1 || f.followed[0] != f.run.ConversationID {
		t.Errorf("followed = %v", f.followed)
	}
}

func TestCompletePushedOpensPRAndReportsLink(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "done it", map[string]string{"soemii/b": branch, "soemii/a": branch})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.host.prs) != 2 {
		t.Fatalf("prs = %v", f.host.prs)
	}
	pr := f.host.prs[0]
	if pr.Repo != "soemii/a" || pr.Head != branch || pr.Base != "main" || pr.Title != "ORC-12: Fix it" ||
		!strings.Contains(pr.Body, "done it") || !strings.Contains(pr.Body, "[Timeline](https://orc.example/runs/"+f.run.ID.String()+")") {
		t.Errorf("pr = %v", pr)
	}
	if f.host.prs[1].Base != "develop" {
		t.Errorf("base = %s", f.host.prs[1].Base)
	}
	r := f.s.runs[f.run.ID]
	if r.State != router.StateSucceeded || len(r.PRURLs) != 2 {
		t.Errorf("run = %+v", r)
	}
	want := "result: Fertig. Pull Request: https://gh/soemii/a/pull/1\nhttps://gh/soemii/b/pull/1"
	if len(f.rep.milestones) != 1 || f.rep.milestones[0] != want {
		t.Errorf("milestones = %q", f.rep.milestones)
	}
	if len(f.followed) != 1 {
		t.Errorf("followed = %v", f.followed)
	}
}

func TestCompletePRFailureMarksFailed(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	f.host.prErr = errors.New("422 boom")
	if err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", map[string]string{"soemii/a": branch}); err != nil {
		t.Fatal(err)
	}
	if f.state() != router.StateFailed {
		t.Errorf("state = %s", f.state())
	}
	if len(f.rep.milestones) != 1 || !strings.HasPrefix(f.rep.milestones[0], "result: Abgebrochen: ") || !strings.Contains(f.rep.milestones[0], "422 boom") {
		t.Errorf("milestones = %v", f.rep.milestones)
	}
	if len(f.followed) != 1 {
		t.Errorf("followed = %v", f.followed)
	}
}

func TestCompleteFailedReportsReason(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_FAILED, "timeout", nil); err != nil {
		t.Fatal(err)
	}
	if f.state() != router.StateFailed || f.rep.milestones[0] != "result: Abgebrochen: timeout" {
		t.Errorf("state=%s milestones=%v", f.state(), f.rep.milestones)
	}
}

func TestCompleteWaitingDoesNotStartFollowUp(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_WAITING, "which branch?", nil); err != nil {
		t.Fatal(err)
	}
	if f.state() != router.StateWaitingForInput || f.rep.milestones[0] != "question: which branch?" || len(f.followed) != 0 {
		t.Errorf("state=%s milestones=%v followed=%v", f.state(), f.rep.milestones, f.followed)
	}
}

func TestCompleteAnsweredStartsFollowUp(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	_ = f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "a", nil)
	if len(f.followed) != 1 {
		t.Errorf("followed = %v", f.followed)
	}
}

func TestCompleteFollowUpErrorIsNotReturned(t *testing.T) {
	f := newFixture(t, "question")
	f.o.followUp = func(context.Context, uuid.UUID) error { return errors.New("boom") }
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "a", nil); err != nil {
		t.Fatal(err)
	}
}

func TestCompleteStoreFailureSkipsMilestone(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	f.s.failUpdate = true
	if err := f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "a", nil); err == nil {
		t.Fatal("want error")
	}
	if len(f.rep.milestones) != 0 || len(f.followed) != 0 {
		t.Error("side effects after failed update")
	}
}

func TestRecordEventRedactsSecrets(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	secret := "ghp_" + strings.Repeat("a", 36)
	ev := &pluginv1.AgentEvent{Type: "message", Text: "token " + secret}
	if err := f.o.RecordEvent(context.Background(), "w1", f.run.ID, ev); err != nil {
		t.Fatal(err)
	}
	last := f.s.events[len(f.s.events)-1]
	raw, ok := last.payload.(json.RawMessage)
	if !ok || last.typ != "message" {
		t.Fatalf("event = %+v", last)
	}
	if strings.Contains(string(raw), secret) || !strings.Contains(string(raw), "token ****") {
		t.Errorf("payload = %s", raw)
	}
}

func TestRecordEventForeignWorkerDenied(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	n := len(f.s.events)
	err := f.o.RecordEvent(context.Background(), "x", f.run.ID, &pluginv1.AgentEvent{Type: "message"})
	if !errors.Is(err, ErrNotLeaseOwner) || len(f.s.events) != n {
		t.Fatalf("err = %v", err)
	}
}

func tokenText() (string, string) {
	secret := "ghp_" + strings.Repeat("d", 36)
	return secret, "see " + secret
}

func TestCompletePushedRedactsResultAndPRBody(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	secret, text := tokenText()
	if err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, text, map[string]string{"soemii/a": branch}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.host.prs[0].Body, secret) || !strings.Contains(f.host.prs[0].Body, "see ****") {
		t.Errorf("body = %s", f.host.prs[0].Body)
	}
	if got := f.s.runs[f.run.ID].Result; got != "see ****" {
		t.Errorf("result = %q", got)
	}
}

func TestCompleteFailedRedactsResult(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	secret, text := tokenText()
	_ = f.complete("w1", workerv1.CompleteRunRequest_FAILED, text, nil)
	if got := f.s.runs[f.run.ID].Result; strings.Contains(got, secret) || got != "see ****" {
		t.Errorf("result = %q", got)
	}
	if strings.Contains(f.rep.milestones[0], secret) {
		t.Errorf("milestone = %q", f.rep.milestones[0])
	}
}

func assertUnchanged(t *testing.T, f *fixture) {
	t.Helper()
	if len(f.host.prs) != 0 || f.state() != router.StateRunning || len(f.rep.milestones) != 0 || len(f.followed) != 0 {
		t.Errorf("run changed: prs=%d state=%s", len(f.host.prs), f.state())
	}
}

func TestCompletePushedOnQuestionDenied(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", map[string]string{"soemii/a": branch})
	if !errors.Is(err, ErrWriteNotAllowed) {
		t.Fatalf("err = %v", err)
	}
	assertUnchanged(t, f)
}

func TestCompletePushedForeignBranchRejected(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", map[string]string{"soemii/a": "main"})
	if !errors.Is(err, ErrBranchMismatch) {
		t.Fatalf("err = %v", err)
	}
	assertUnchanged(t, f)
}

func TestCompletePushedWithoutBranchesRejected(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", nil); !errors.Is(err, ErrInvalidOutcome) {
		t.Fatalf("err = %v", err)
	}
	assertUnchanged(t, f)
}

func TestCompleteTwiceOpensPROnce(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	b := map[string]string{"soemii/a": branch}
	if err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", b); err != nil {
		t.Fatal(err)
	}
	if err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", b); !errors.Is(err, ErrNotLeaseOwner) {
		t.Fatalf("err = %v", err)
	}
	if len(f.host.prs) != 1 {
		t.Errorf("prs = %d", len(f.host.prs))
	}
}

func TestCompletePartialPRFailureKeepsOpenedURLs(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	f.host.failRepo = "soemii/b"
	err := f.complete("w1", workerv1.CompleteRunRequest_PUSHED, "x", map[string]string{"soemii/a": branch, "soemii/b": branch})
	if err != nil {
		t.Fatal(err)
	}
	r := f.s.runs[f.run.ID]
	if r.State != router.StateFailed || len(r.PRURLs) != 1 || r.PRURLs[0] != "https://gh/soemii/a/pull/1" {
		t.Errorf("run = %+v", r)
	}
	if !strings.Contains(f.rep.milestones[0], "https://gh/soemii/a/pull/1") {
		t.Errorf("milestone = %q", f.rep.milestones[0])
	}
}

func TestRecordEventRejectsUnknownType(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	n := len(f.s.events)
	err := f.o.RecordEvent(context.Background(), "w1", f.run.ID, &pluginv1.AgentEvent{Type: "evil"})
	if !errors.Is(err, ErrInvalidEvent) || len(f.s.events) != n {
		t.Fatalf("err = %v", err)
	}
}

func TestCompleteAnsweredEmptyTextPostsFallback(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	if err := f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "  ", nil); err != nil {
		t.Fatal(err)
	}
	if f.state() != router.StateSucceeded || len(f.rep.milestones) != 1 || f.rep.milestones[0] != "result: "+textNoAnswer {
		t.Errorf("state=%s milestones=%v", f.state(), f.rep.milestones)
	}
}

func TestCompleteWaitingStoresRedactedQuestionAsAgentInput(t *testing.T) {
	f := newFixture(t, "task")
	f.lease(t)
	_, text := tokenText()
	if err := f.complete("w1", workerv1.CompleteRunRequest_WAITING, text, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.s.inputs) != 1 {
		t.Fatalf("inputs = %+v", f.s.inputs)
	}
	in := f.s.inputs[0]
	if !in.FromAgent || in.ActorEmail != "" || in.Body != "see ****" || in.RunID == nil || *in.RunID != f.run.ID ||
		in.ConversationID != f.run.ConversationID || in.Tenant != "acme" || !in.CreatedAt.Equal(f.now) || in.ID == uuid.Nil {
		t.Errorf("input = %+v", in)
	}
}

func TestCompleteAnsweredStoresNoAgentInput(t *testing.T) {
	f := newFixture(t, "question")
	f.lease(t)
	_ = f.complete("w1", workerv1.CompleteRunRequest_ANSWERED, "42", nil)
	if len(f.s.inputs) != 0 {
		t.Errorf("inputs = %+v", f.s.inputs)
	}
}
