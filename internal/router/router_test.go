package router

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/classifier"
	"github.com/soemii/orchivio-server/internal/config"

	pluginv1 "github.com/soemii/orchivio-server/contract/gen/orchivio/plugin/v1"
)

const actor = "you@example.com"

type fakeStore struct {
	seen   map[string]bool
	convs  []*Conversation
	inputs []*Input
	runs   []*Run
	events []string

	markErr, addErr error
}

func (s *fakeStore) MarkInbound(_ context.Context, source, id string) (bool, error) {
	if s.markErr != nil {
		return false, s.markErr
	}
	k := source + "/" + id
	if s.seen[k] {
		return false, nil
	}
	s.seen[k] = true
	return true, nil
}
func (s *fakeStore) FindConversation(_ context.Context, tenant, ref string) (*Conversation, error) {
	for _, c := range s.convs {
		if c.Tenant == tenant && c.TicketRef == ref {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *fakeStore) GetConversation(_ context.Context, id uuid.UUID) (*Conversation, error) {
	for _, c := range s.convs {
		if c.ID == id {
			cp := *c
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("conversation %s not found", id)
}
func (s *fakeStore) CreateConversation(_ context.Context, c Conversation) error {
	s.convs = append(s.convs, &c)
	return nil
}
func (s *fakeStore) AddInput(_ context.Context, in Input) error {
	if s.addErr != nil {
		return s.addErr
	}
	s.inputs = append(s.inputs, &in)
	return nil
}
func (s *fakeStore) OpenInputs(_ context.Context, cid uuid.UUID) ([]Input, error) {
	var out []Input
	for _, in := range s.inputs {
		if in.ConversationID == cid && in.RunID == nil {
			out = append(out, *in)
		}
	}
	return out, nil
}
func (s *fakeStore) ActiveRun(_ context.Context, cid uuid.UUID) (*Run, error) {
	for _, r := range s.runs {
		if r.ConversationID == cid && (r.State == StateClassifying || r.State == StateQueued || r.State == StateRunning ||
			r.State == StatePushing || r.State == StateWaitingForInput) {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}
func (s *fakeStore) LastRun(_ context.Context, cid uuid.UUID) (*Run, error) {
	var last *Run
	for _, r := range s.runs {
		if r.ConversationID == cid {
			cp := *r
			last = &cp
		}
	}
	return last, nil
}
func (s *fakeStore) CreateRun(_ context.Context, r Run) error {
	s.runs = append(s.runs, &r)
	return nil
}
func (s *fakeStore) UpdateRun(_ context.Context, r Run) error {
	for i, x := range s.runs {
		if x.ID == r.ID {
			s.runs[i] = &r
		}
	}
	return nil
}
func (s *fakeStore) ClaimInputs(_ context.Context, cid, rid uuid.UUID) error {
	for _, in := range s.inputs {
		if in.ConversationID == cid && in.RunID == nil {
			id := rid
			in.RunID = &id
		}
	}
	return nil
}
func (s *fakeStore) History(_ context.Context, cid uuid.UUID) ([]HistoryEntry, error) {
	var out []HistoryEntry
	for _, in := range s.inputs {
		if in.ConversationID == cid {
			out = append(out, HistoryEntry{At: in.CreatedAt, Author: in.ActorEmail, Text: in.Body})
		}
	}
	return out, nil
}
func (s *fakeStore) AppendEvent(_ context.Context, _ string, _ uuid.UUID, typ string, payload any) error {
	s.events = append(s.events, fmt.Sprintf("%s %v", typ, payload))
	return nil
}

type fakeTracker struct {
	labels []string
	err    error
}

func (f *fakeTracker) GetTicket(_ context.Context, _, ref string) (*pluginv1.Ticket, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &pluginv1.Ticket{TicketRef: ref, Title: "Titel", Description: "Beschreibung", Labels: f.labels}, nil
}

type fakeReporter struct {
	milestones, notices []string
	failMilestone       string
}

func (f *fakeReporter) Milestone(_ context.Context, _ Run, m, text string) error {
	if m == f.failMilestone {
		return errors.New("milestone boom")
	}
	f.milestones = append(f.milestones, m+": "+text)
	return nil
}
func (f *fakeReporter) Notice(_ context.Context, _, _, text string) error {
	f.notices = append(f.notices, text)
	return nil
}

type fixture struct {
	r   *Router
	s   *fakeStore
	tr  *fakeTracker
	rep *fakeReporter
}

func newFixture(t *testing.T, defaultRepo string) *fixture {
	t.Helper()
	dir := t.TempDir()
	yaml := fmt.Sprintf(`tenant: acme
tracker: { name: linear, address: localhost:7101 }
codeHost: { name: github, address: localhost:7201 }
profiles:
  default: { runtime: claude-code, model: m, maxDuration: 20m }
projects:
  - name: demo
    trackerKey: ORC
    profile: default
    repos:
      - { slug: soemii/playground, baseBranch: main, allowedPaths: [""] }
    classifier:
      taskLabels: ["orchivio:aufgabe"]
      questionLabels: ["orchivio:frage"]
      repoLabels: { "repo:playground": soemii/playground }
      defaultRepo: %q
    roles: { questions: ["%s"], tasks: ["%s"] }
    maxDiffLines: 500
`, defaultRepo, actor, actor)
	if err := os.WriteFile(filepath.Join(dir, "acme.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	sync, err := config.NewSync(dir, time.Hour, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{s: &fakeStore{seen: map[string]bool{}}, tr: &fakeTracker{}, rep: &fakeReporter{}}
	var n uint64
	newID := func() uuid.UUID {
		n++
		var id uuid.UUID
		binary.BigEndian.PutUint64(id[8:], n)
		id[0] = 0xab
		return id
	}
	clock := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	f.r = New(sync, f.s, f.tr, f.rep, func() time.Time { clock = clock.Add(time.Minute); return clock }, newID)
	return f
}

func assigned(id, email string) *pluginv1.TrackerEvent {
	return &pluginv1.TrackerEvent{ExternalId: id, Kind: pluginv1.TrackerEvent_ASSIGNED, TicketRef: "ORC-12", ProjectKey: "ORC",
		Actor: &pluginv1.Actor{Email: email}}
}

func comment(id, body string, mention bool) *pluginv1.TrackerEvent {
	return &pluginv1.TrackerEvent{ExternalId: id, Kind: pluginv1.TrackerEvent_COMMENTED, TicketRef: "ORC-12", ProjectKey: "ORC",
		Actor: &pluginv1.Actor{Email: actor}, Body: body, MentionsBot: mention}
}

func handle(t *testing.T, f *fixture, ev *pluginv1.TrackerEvent) {
	t.Helper()
	if err := f.r.Handle(context.Background(), "acme", ev); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}

func TestHandleIgnoresDuplicateEvent(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", actor))
	handle(t, f, assigned("e1", actor))
	if len(f.s.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.s.runs))
	}
}

func TestHandleRejectsActorWithoutRole(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", "stranger@example.com"))
	if len(f.rep.notices) != 1 || f.rep.notices[0] != NoticeMissingRole {
		t.Fatalf("notices = %v", f.rep.notices)
	}
	if len(f.s.convs) != 0 || len(f.s.runs) != 0 || len(f.s.inputs) != 0 {
		t.Fatal("nothing must be stored")
	}
}

func TestHandleAssignedCreatesQueuedTask(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", actor))
	run := f.s.runs[0]
	if run.State != StateQueued || run.Kind != classifier.KindTask {
		t.Fatalf("state=%s kind=%s", run.State, run.Kind)
	}
	if want := "orchivio/orc-12-" + run.ID.String()[:8]; run.Branch != want {
		t.Fatalf("branch = %q, want %q", run.Branch, want)
	}
	if len(f.rep.milestones) != 1 || f.rep.milestones[0] != "started: Gestartet als Aufgabe in soemii/playground." {
		t.Fatalf("milestones = %v", f.rep.milestones)
	}
	if !strings.Contains(run.Prompt, "# ORC-12: Titel") {
		t.Fatalf("prompt = %s", run.Prompt)
	}
	if f.s.inputs[0].RunID == nil || *f.s.inputs[0].RunID != run.ID {
		t.Fatal("input not claimed")
	}
}

func TestHandleCommentWithoutMentionAndConversationIgnored(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, comment("e1", "hallo", false))
	if len(f.s.convs) != 0 || len(f.s.runs) != 0 || len(f.rep.notices) != 0 {
		t.Fatal("must be ignored")
	}
}

func TestHandleCommentDuringRunningRunDefersInput(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", actor))
	f.s.runs[0].State = StateRunning
	handle(t, f, comment("e2", "noch was", false))
	if len(f.s.runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(f.s.runs))
	}
	open, _ := f.s.OpenInputs(context.Background(), f.s.convs[0].ID)
	if len(open) != 1 || open[0].Body != "noch was" {
		t.Fatalf("open inputs = %v", open)
	}
}

func TestHandleNeedsRepoWaits(t *testing.T) {
	f := newFixture(t, "")
	handle(t, f, assigned("e1", actor))
	if f.s.runs[0].State != StateWaitingForInput {
		t.Fatalf("state = %s", f.s.runs[0].State)
	}
	if len(f.rep.milestones) != 1 || f.rep.milestones[0] != "question: "+TextWhichRepo {
		t.Fatalf("milestones = %v", f.rep.milestones)
	}
}

func TestHandleAnswerResumesWaitingRun(t *testing.T) {
	f := newFixture(t, "")
	handle(t, f, assigned("e1", actor))
	waiting := f.s.runs[0].ID
	f.tr.labels = []string{"repo:playground"}
	handle(t, f, comment("e2", "Das Playground-Repo", false))
	if len(f.s.runs) != 1 || f.s.runs[0].ID != waiting {
		t.Fatalf("expected the same single run, got %d", len(f.s.runs))
	}
	run := f.s.runs[0]
	if run.State != StateQueued || !strings.Contains(run.Prompt, "Das Playground-Repo") {
		t.Fatalf("state=%s prompt=%s", run.State, run.Prompt)
	}
}

func TestStartFollowUpUsesOpenInputs(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", actor))
	first := f.s.runs[0]
	f.s.runs[0].State = StateRunning
	handle(t, f, comment("e2", "bitte noch X", false))
	f.s.runs[0].State = StateSucceeded
	cid := f.s.convs[0].ID

	if err := f.r.StartFollowUp(context.Background(), cid); err != nil {
		t.Fatal(err)
	}
	if len(f.s.runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(f.s.runs))
	}
	second := f.s.runs[1]
	if second.State != StateQueued || second.Kind != classifier.KindTask || second.Branch != first.Branch {
		t.Fatalf("second = %+v", second)
	}
	if !strings.Contains(second.Prompt, "bitte noch X") {
		t.Fatal("prompt lacks follow-up input")
	}

	second.State = StateSucceeded
	if err := f.r.StartFollowUp(context.Background(), cid); err != nil {
		t.Fatal(err)
	}
	if len(f.s.runs) != 2 {
		t.Fatalf("no open inputs must not start a run, runs = %d", len(f.s.runs))
	}
}

func TestHandleGetTicketErrorFailsRunAndLaterCommentStartsNewRun(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	f.tr.err = errors.New("tracker down")

	err := f.r.Handle(context.Background(), "acme", assigned("e1", actor))

	if err == nil || !errors.Is(err, f.tr.err) {
		t.Fatalf("err = %v", err)
	}
	if f.s.runs[0].State != StateFailed || len(f.rep.milestones) != 0 {
		t.Fatalf("state=%s milestones=%v", f.s.runs[0].State, f.rep.milestones)
	}
	f.tr.err = nil
	handle(t, f, comment("e2", "nochmal", false))
	if len(f.s.runs) != 2 || f.s.runs[1].State != StateQueued {
		t.Fatalf("runs = %d", len(f.s.runs))
	}
}

func TestHandleStartedMilestoneErrorFailsRun(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	f.rep.failMilestone = "started"
	if err := f.r.Handle(context.Background(), "acme", assigned("e1", actor)); err == nil {
		t.Fatal("expected error")
	}
	if f.s.runs[0].State != StateFailed {
		t.Fatalf("state = %s", f.s.runs[0].State)
	}
}

func TestHandleMarkInboundErrorDoesNothingElse(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	f.s.markErr = errors.New("db down")
	if err := f.r.Handle(context.Background(), "acme", assigned("e1", actor)); err == nil {
		t.Fatal("expected error")
	}
	if len(f.s.convs)+len(f.s.inputs)+len(f.s.runs)+len(f.rep.notices)+len(f.rep.milestones) != 0 {
		t.Fatal("no side effects expected")
	}
}

func TestHandleAddInputErrorStartsNoRun(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	f.s.addErr = errors.New("db down")
	if err := f.r.Handle(context.Background(), "acme", assigned("e1", actor)); err == nil {
		t.Fatal("expected error")
	}
	if len(f.s.runs) != 0 || len(f.rep.milestones) != 0 {
		t.Fatal("no run expected")
	}
}

func TestHandleRolelessFollowupIsSilentlyIgnored(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, assigned("e1", actor))
	f.s.runs[0].State = StateSucceeded
	ev := comment("e2", "hi", false)
	ev.Actor.Email = "stranger@example.com"

	handle(t, f, ev)

	if len(f.rep.notices) != 0 || len(f.s.inputs) != 1 || len(f.s.runs) != 1 {
		t.Fatalf("notices=%d inputs=%d runs=%d", len(f.rep.notices), len(f.s.inputs), len(f.s.runs))
	}
}

func TestHandleMentionWithoutConversationCreatesQueuedQuestion(t *testing.T) {
	f := newFixture(t, "soemii/playground")
	handle(t, f, comment("e1", "@orchivio was ist das?", true))
	if len(f.s.convs) != 1 || len(f.s.runs) != 1 {
		t.Fatalf("convs=%d runs=%d", len(f.s.convs), len(f.s.runs))
	}
	if r := f.s.runs[0]; r.State != StateQueued || r.Kind != classifier.KindQuestion || r.Branch != "" {
		t.Fatalf("run = %+v", r)
	}
}
