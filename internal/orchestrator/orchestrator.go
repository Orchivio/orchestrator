// Package orchestrator is the server side of the run lifecycle: workers
// lease runs, stream events, fetch repo credentials and complete runs.
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/contract/redact"
	"github.com/soemii/orchivio-server/internal/config"
	"github.com/soemii/orchivio-server/internal/router"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
	workerv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/worker/v1"
)

var (
	// ErrNotLeaseOwner is returned when the worker does not hold the run's lease.
	ErrNotLeaseOwner = errors.New("worker does not own the lease of this run")
	// ErrWriteNotAllowed is returned for write credentials on non-task runs.
	ErrWriteNotAllowed = errors.New("write credentials are only issued for task runs")
	// ErrInvalidOutcome is returned for an unspecified or unknown outcome.
	ErrInvalidOutcome = errors.New("invalid run outcome")
	// ErrUnknownRun is returned when the run id does not exist.
	ErrUnknownRun = errors.New("unknown run")
	// ErrBranchMismatch is returned when a pushed branch is not the run's branch.
	ErrBranchMismatch = errors.New("pushed branch does not match the run branch")
	// ErrInvalidEvent is returned for event types outside the normalized set.
	ErrInvalidEvent = errors.New("invalid event type")
)

const (
	leaseGrace          = 5 * time.Minute
	eventRunState       = "run.state"
	kindTask            = "task"
	milestoneQuestion   = config.MilestoneQuestion
	milestoneResult     = config.MilestoneResult
	abortedPrefix       = "Abgebrochen: "
	donePrefix          = "Fertig. Pull Request: "
	redactedFallbackFmt = `{"type":%q,"redacted":true}`
)

var eventTypes = []string{"run.started", "tool.called", "tool.finished", "file.changed", "message",
	"question.asked", "usage", "run.finished"}

// Orchestrator implements the worker-facing run lifecycle.
type Orchestrator struct {
	cfg       *config.Sync
	store     Store
	codeHost  CodeHost
	tracker   router.Tracker
	reporter  router.Reporter
	followUp  func(ctx context.Context, conversationID uuid.UUID) error
	now       func() time.Time
	leaseFor  time.Duration
	publicURL string
	// ponytail: global lock, conditional UPDATE ... WHERE state='running' if more than one server process
	completeMu sync.Mutex
}

// New wires the orchestrator. leaseFor is the lease length used when the
// run's profile is unknown.
func New(cfg *config.Sync, s Store, ch CodeHost, tracker router.Tracker, rep router.Reporter,
	followUp func(ctx context.Context, conversationID uuid.UUID) error, now func() time.Time,
	leaseFor time.Duration, publicURL string) *Orchestrator {
	return &Orchestrator{cfg: cfg, store: s, codeHost: ch, tracker: tracker, reporter: rep,
		followUp: followUp, now: now, leaseFor: leaseFor, publicURL: strings.TrimRight(publicURL, "/")}
}

// Lease hands the next queued run to the worker; nil when the queue is empty.
func (o *Orchestrator) Lease(ctx context.Context, workerID string) (*workerv1.RunAssignment, error) {
	run, err := o.store.LeaseNext(ctx, workerID, o.now().Add(o.leaseFor))
	if err != nil || run == nil {
		return nil, err
	}
	_, project, found := o.cfg.Current().ProjectFor(run.Tenant, run.Project)
	profile, hasProfile := o.profile(run)
	if hasProfile {
		until := o.now().Add(profile.MaxDuration + leaseGrace)
		run.LeaseUntil = &until
		if err := o.store.UpdateRun(ctx, *run); err != nil {
			return nil, fmt.Errorf("extend lease of run %s: %w", run.ID, err)
		}
	}
	o.recordState(ctx, run)
	a := &workerv1.RunAssignment{
		RunId: run.ID.String(), Tenant: run.Tenant, Kind: run.Kind, Branch: run.Branch,
		TicketRef: run.TicketRef, TicketTitle: o.ticketTitle(ctx, run),
		Spec: &pluginv1.RunSpec{RunId: run.ID.String(), Prompt: run.Prompt, Model: profile.Model, Instructions: profile.Instructions},
	}
	if run.LeaseUntil != nil {
		a.LeaseUntil = timestamppb.New(*run.LeaseUntil)
	}
	if hasProfile {
		a.MaxDuration = durationpb.New(profile.MaxDuration)
	}
	if found {
		a.MaxDiffLines = int32(project.MaxDiffLines)
		a.Repos, a.AllowedPaths = assignmentRepos(run, project)
	}
	return a, nil
}

func assignmentRepos(run *router.Run, project config.Project) ([]*workerv1.Repo, []string) {
	var repos []*workerv1.Repo
	var paths []string
	for _, slug := range run.Repos {
		repo, _ := findRepo(project, slug)
		repos = append(repos, &workerv1.Repo{Slug: slug, BaseBranch: repo.BaseBranch})
		for _, p := range repo.AllowedPaths {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
	}
	return repos, paths
}

func findRepo(project config.Project, slug string) (config.Repo, bool) {
	for _, r := range project.Repos {
		if r.Slug == slug {
			return r, true
		}
	}
	return config.Repo{}, false
}

func (o *Orchestrator) profile(run *router.Run) (config.Profile, bool) {
	tenant, project, ok := o.cfg.Current().ProjectFor(run.Tenant, run.Project)
	if !ok {
		return config.Profile{}, false
	}
	p, ok := tenant.Profiles[project.Profile]
	return p, ok
}

// ticketTitle is best effort: a tracker outage must not block a lease or PR.
func (o *Orchestrator) ticketTitle(ctx context.Context, run *router.Run) string {
	t, err := o.tracker.GetTicket(ctx, run.Tenant, run.TicketRef)
	if err != nil || t == nil {
		slog.Warn("ticket title unavailable", "run", run.ID, "ticket", run.TicketRef, "error", err)
		return ""
	}
	return t.GetTitle()
}

// RecordEvent stores an agent event after masking secrets.
func (o *Orchestrator) RecordEvent(ctx context.Context, workerID string, runID uuid.UUID, ev *pluginv1.AgentEvent) error {
	run, err := o.owned(ctx, workerID, runID)
	if err != nil {
		return err
	}
	if !slices.Contains(eventTypes, ev.GetType()) {
		return fmt.Errorf("run %s: %w: %q", runID, ErrInvalidEvent, ev.GetType())
	}
	raw, err := protojson.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event of run %s: %w", runID, err)
	}
	text := redact.String(string(raw))
	payload := json.RawMessage(text)
	if !json.Valid(payload) {
		payload = json.RawMessage(fmt.Sprintf(redactedFallbackFmt, ev.GetType()))
	}
	return o.store.AppendEvent(ctx, run.Tenant, run.ID, ev.GetType(), payload)
}

// Credentials issues repo credentials for the run's repos.
func (o *Orchestrator) Credentials(ctx context.Context, workerID string, runID uuid.UUID, write bool) (*pluginv1.RepoCredentials, error) {
	run, err := o.owned(ctx, workerID, runID)
	if err != nil {
		return nil, err
	}
	if write && run.Kind != kindTask {
		return nil, ErrWriteNotAllowed
	}
	return o.codeHost.IssueRepoCredentials(ctx, run.Tenant, run.Repos, write)
}

// owned loads the run and checks the worker holds its lease.
func (o *Orchestrator) owned(ctx context.Context, workerID string, runID uuid.UUID) (*router.Run, error) {
	run, err := o.store.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run %s: %w", runID, err)
	}
	if run == nil {
		return nil, fmt.Errorf("run %s: %w", runID, ErrUnknownRun)
	}
	if run.LeaseOwner != workerID || run.State != router.StateRunning {
		return nil, ErrNotLeaseOwner
	}
	return run, nil
}

// Complete finishes a run according to the worker's outcome. Agent text is
// redacted once here and only the redacted form is stored or forwarded.
func (o *Orchestrator) Complete(ctx context.Context, req *workerv1.CompleteRunRequest) error {
	o.completeMu.Lock()
	defer o.completeMu.Unlock()
	runID, err := uuid.Parse(req.GetRunId())
	if err != nil {
		return fmt.Errorf("run id %q: %w", req.GetRunId(), err)
	}
	run, err := o.owned(ctx, req.GetWorkerId(), runID)
	if err != nil {
		return err
	}
	run.SessionID = req.GetSessionId()
	text := redact.String(req.GetText())
	switch req.GetOutcome() {
	case workerv1.CompleteRunRequest_ANSWERED:
		err = o.finish(ctx, run, router.StateSucceeded, text, milestoneResult, text)
	case workerv1.CompleteRunRequest_PUSHED:
		if err = validatePushed(run, req); err == nil {
			err = o.completePushed(ctx, run, req.GetPushedBranches(), text)
		}
	case workerv1.CompleteRunRequest_WAITING:
		err = o.finish(ctx, run, router.StateWaitingForInput, text, milestoneQuestion, text)
	case workerv1.CompleteRunRequest_FAILED:
		err = o.finish(ctx, run, router.StateFailed, text, milestoneResult, abortedPrefix+text)
	default:
		return fmt.Errorf("run %s: %w: %v", runID, ErrInvalidOutcome, req.GetOutcome())
	}
	if err != nil {
		return err
	}
	if run.State != router.StateWaitingForInput {
		if err := o.followUp(ctx, run.ConversationID); err != nil {
			slog.Error("follow-up after run failed", "run", run.ID, "error", err)
		}
	}
	return nil
}

func validatePushed(run *router.Run, req *workerv1.CompleteRunRequest) error {
	if run.Kind != kindTask {
		return ErrWriteNotAllowed
	}
	if len(req.GetPushedBranches()) == 0 {
		return fmt.Errorf("run %s: %w: PUSHED without branches", run.ID, ErrInvalidOutcome)
	}
	for slug, branch := range req.GetPushedBranches() {
		if branch != run.Branch {
			return fmt.Errorf("run %s repo %s: %w", run.ID, slug, ErrBranchMismatch)
		}
	}
	return nil
}

func (o *Orchestrator) completePushed(ctx context.Context, run *router.Run, branches map[string]string, text string) error {
	run.State = router.StatePushing
	run.Result = text
	if err := o.store.UpdateRun(ctx, *run); err != nil {
		return fmt.Errorf("update run %s: %w", run.ID, err)
	}
	o.recordState(ctx, run)
	urls, err := o.openPullRequests(ctx, run, branches, text)
	run.PRURLs = urls
	if err != nil {
		reason := redact.String(err.Error())
		if len(urls) > 0 {
			reason += "\nBereits geöffnet: " + strings.Join(urls, "\n")
		}
		return o.finish(ctx, run, router.StateFailed, reason, milestoneResult, abortedPrefix+reason)
	}
	return o.finish(ctx, run, router.StateSucceeded, text, milestoneResult, donePrefix+strings.Join(urls, "\n"))
}

func (o *Orchestrator) openPullRequests(ctx context.Context, run *router.Run, branches map[string]string, text string) ([]string, error) {
	_, project, _ := o.cfg.Current().ProjectFor(run.Tenant, run.Project)
	title := run.TicketRef + ": " + o.ticketTitle(ctx, run)
	body := text + fmt.Sprintf("\n\n[Timeline](%s/runs/%s)", o.publicURL, run.ID)
	slugs := make([]string, 0, len(branches))
	for slug := range branches {
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)
	var urls []string
	for _, slug := range slugs {
		repo, ok := findRepo(project, slug)
		if !ok {
			return urls, fmt.Errorf("repo %s is not configured for project %s", slug, run.Project)
		}
		pr, err := o.codeHost.OpenPullRequest(ctx, run.Tenant, &pluginv1.OpenPullRequestRequest{
			Repo: slug, Base: repo.BaseBranch, Head: branches[slug], Title: title, Body: body})
		if err != nil {
			return urls, fmt.Errorf("open pull request in %s: %w", slug, err)
		}
		urls = append(urls, pr.GetUrl())
	}
	return urls, nil
}

// finish persists the final state, then reports the milestone. A failing
// milestone is logged only: the state is already durable.
func (o *Orchestrator) finish(ctx context.Context, run *router.Run, state, result, milestone, text string) error {
	run.State = state
	run.Result = result
	if err := o.store.UpdateRun(ctx, *run); err != nil {
		return fmt.Errorf("update run %s: %w", run.ID, err)
	}
	o.recordState(ctx, run)
	if err := o.reporter.Milestone(ctx, *run, milestone, text); err != nil {
		slog.Error("milestone failed", "run", run.ID, "milestone", milestone, "error", err)
	}
	return nil
}

func (o *Orchestrator) recordState(ctx context.Context, run *router.Run) {
	if err := o.store.AppendEvent(ctx, run.Tenant, run.ID, eventRunState, map[string]string{"state": run.State}); err != nil {
		slog.Error("record state event failed", "run", run.ID, "error", err)
	}
}
