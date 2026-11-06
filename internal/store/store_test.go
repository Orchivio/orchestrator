package store

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/router"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	testURL string
	base    = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
)

func TestMain(m *testing.M) {
	ctx := context.Background()
	ctr, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("orchivio"), tcpostgres.WithUsername("u"), tcpostgres.WithPassword("p"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		panic(err)
	}
	testURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = ctr.Terminate(ctx)
	os.Exit(code)
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), testURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func newConversation(t *testing.T, s *Store) router.Conversation {
	t.Helper()
	c := router.Conversation{ID: uuid.New(), Tenant: "t-" + uuid.NewString(), Project: "ORC", TicketRef: "ORC-1", CreatedAt: base}
	if err := s.CreateConversation(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func newRun(t *testing.T, s *Store, c router.Conversation, state string, created time.Time) router.Run {
	t.Helper()
	r := router.Run{ID: uuid.New(), Tenant: c.Tenant, ConversationID: c.ID, Project: c.Project, TicketRef: c.TicketRef,
		Kind: "task", State: state, Repos: []string{"a/b"}, CreatedAt: created, UpdatedAt: created}
	if err := s.CreateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestMarkInboundIsIdempotent(t *testing.T) {
	s, ctx, id := newStore(t), context.Background(), uuid.NewString()
	first, err := s.MarkInbound(ctx, "linear", id)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.MarkInbound(ctx, "linear", id)
	if err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("got %v, %v; want true, false", first, second)
	}
}

func TestLeaseSkipsLockedAndExpired(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	s.now = func() time.Time { return base }
	// Leases are global, so drain whatever other tests left queued.
	for {
		r, err := s.LeaseNext(ctx, "drain", base.Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			break
		}
	}
	c := newConversation(t, s)
	r1 := newRun(t, s, c, router.StateQueued, base)
	newRun(t, s, c, router.StateQueued, base.Add(time.Second))
	until := base.Add(time.Minute)

	got := make([]*router.Run, 2)
	var wg sync.WaitGroup
	for i, w := range []string{"w1", "w2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			if got[i], err = s.LeaseNext(ctx, w, until); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got[0] == nil || got[1] == nil || got[0].ID == got[1].ID {
		t.Fatalf("workers must lease two different runs: %+v %+v", got[0], got[1])
	}
	if got[0].State != router.StateRunning || got[0].LeaseOwner == "" {
		t.Fatalf("leased run not running/owned: %+v", got[0])
	}
	if third, err := s.LeaseNext(ctx, "w3", until); err != nil || third != nil {
		t.Fatalf("third lease = %+v, %v; want nil", third, err)
	}

	// Lease of r1 ran out before "now"=base+2min -> re-leased; r2 equally.
	later := until.Add(time.Minute)
	s.now = func() time.Time { return later }
	again, err := s.LeaseNext(ctx, "w4", later.Add(time.Minute))
	if err != nil || again == nil {
		t.Fatalf("expired run not re-leased: %+v, %v", again, err)
	}
	if again.ID != r1.ID || again.LeaseOwner != "w4" {
		t.Fatalf("got %v owner %q, want %v owner w4", again.ID, again.LeaseOwner, r1.ID)
	}
}

func TestEventsAppendOrdered(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	r := newRun(t, s, c, router.StateRunning, base)
	for _, p := range []any{map[string]int{"n": 1}, json.RawMessage(`{"n":2}`), struct{ N int }{3}} {
		if err := s.AppendEvent(ctx, c.Tenant, r.ID, "message", p); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.ListEvents(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"n": 1}`, `{"n": 2}`, `{"N": 3}`}
	if len(evs) != 3 {
		t.Fatalf("got %d events", len(evs))
	}
	for i, e := range evs {
		if e.Type != "message" || string(e.Payload) != want[i] || e.At.IsZero() {
			t.Fatalf("event %d = %+v, want payload %s", i, e, want[i])
		}
	}
}

func TestConversationInputsClaimAndHistory(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)

	found, err := s.FindConversation(ctx, c.Tenant, c.TicketRef)
	if err != nil || found == nil || !reflect.DeepEqual(*found, c) {
		t.Fatalf("FindConversation = %+v, %v; want %+v", found, err, c)
	}
	if none, err := s.FindConversation(ctx, c.Tenant, "nope"); err != nil || none != nil {
		t.Fatalf("unknown ticket = %+v, %v; want nil, nil", none, err)
	}
	if none, err := s.GetConversation(ctx, uuid.New()); err != nil || none != nil {
		t.Fatalf("unknown id = %+v, %v; want nil, nil", none, err)
	}

	in := func(body string, at time.Time) router.Input {
		return router.Input{ID: uuid.New(), Tenant: c.Tenant, ConversationID: c.ID, ActorEmail: "dev@x",
			Body: body, Trigger: "assigned", CreatedAt: at}
	}
	second, first := in("second", base.Add(2*time.Minute)), in("first", base.Add(time.Minute))
	for _, i := range []router.Input{second, first, in("", base.Add(3*time.Minute))} {
		if err := s.AddInput(ctx, i); err != nil {
			t.Fatal(err)
		}
	}
	open, err := s.OpenInputs(ctx, c.ID)
	if err != nil || len(open) != 3 || open[0].ID != first.ID || open[1].ID != second.ID {
		t.Fatalf("OpenInputs = %+v, %v", open, err)
	}

	run := newRun(t, s, c, router.StateSucceeded, base.Add(90*time.Second))
	run.Result, run.UpdatedAt = "done", base.Add(150*time.Second)
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimInputs(ctx, c.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenInputs(ctx, c.ID); len(open) != 0 {
		t.Fatalf("inputs still open after claim: %+v", open)
	}

	hist, err := s.History(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []router.HistoryEntry{
		{At: first.CreatedAt, Author: "dev@x", Text: "first"},
		{At: second.CreatedAt, Author: "dev@x", Text: "second"},
		{At: run.UpdatedAt, Author: "Orchivio", Text: "done", FromAgent: true},
	}
	if !reflect.DeepEqual(hist, want) {
		t.Fatalf("History = %+v\nwant %+v", hist, want)
	}
}

func TestRunRoundTripActiveAndLast(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	if r, err := s.ActiveRun(ctx, c.ID); err != nil || r != nil {
		t.Fatalf("ActiveRun on empty = %+v, %v", r, err)
	}
	old := newRun(t, s, c, router.StateSucceeded, base)
	cur := newRun(t, s, c, router.StateQueued, base.Add(time.Minute))

	cur.State, cur.Branch, cur.Prompt, cur.Result = router.StateWaitingForInput, "orchivio/orc-1-abc", "p", "r"
	cur.Repos, cur.PRURLs, cur.SessionID = []string{"a/b", "c/d"}, []string{"https://pr/1"}, "sess"
	lease := base.Add(time.Hour)
	cur.LeaseOwner, cur.LeaseUntil, cur.UpdatedAt = "w1", &lease, base.Add(2*time.Minute)
	if err := s.UpdateRun(ctx, cur); err != nil {
		t.Fatal(err)
	}

	got, err := s.GetRun(ctx, cur.ID)
	if err != nil || got == nil || !reflect.DeepEqual(*got, cur) {
		t.Fatalf("GetRun = %+v, %v\nwant %+v", got, err, cur)
	}
	if active, _ := s.ActiveRun(ctx, c.ID); active == nil || active.ID != cur.ID {
		t.Fatalf("ActiveRun = %+v, want %v", active, cur.ID)
	}
	if last, _ := s.LastRun(ctx, c.ID); last == nil || last.ID != cur.ID {
		t.Fatalf("LastRun = %+v, want %v", last, cur.ID)
	}
	cur.State = router.StateFailed
	if err := s.UpdateRun(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if active, _ := s.ActiveRun(ctx, c.ID); active != nil {
		t.Fatalf("ActiveRun after fail = %+v, want nil (old %v)", active, old.ID)
	}
	if none, err := s.GetRun(ctx, uuid.New()); err != nil || none != nil {
		t.Fatalf("unknown run = %+v, %v", none, err)
	}
}

func TestNewRunHasEmptyArraysNotNull(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	r := router.Run{ID: uuid.New(), Tenant: c.Tenant, ConversationID: c.ID, Kind: "question",
		State: router.StateClassifying, CreatedAt: base, UpdatedAt: base}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got == nil || got.Repos == nil || got.PRURLs == nil {
		t.Fatalf("GetRun = %+v, %v; want non-nil empty slices", got, err)
	}
}

func TestMigrationURL(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://u:p@h:5432/d?sslmode=disable":   "pgx5://u:p@h:5432/d?sslmode=disable",
		"postgresql://u:p@h:5432/d?sslmode=disable": "pgx5://u:p@h:5432/d?sslmode=disable",
	} {
		got, err := migrationURL(in)
		if err != nil || got != want {
			t.Errorf("migrationURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "pg", "mysql://u:secret@h/d", "http://h"} {
		if _, err := migrationURL(in); err == nil {
			t.Errorf("migrationURL(%q) = nil error, want error", in)
		}
	}
}

func TestUpdateRunUnknownIDFails(t *testing.T) {
	s := newStore(t)
	err := s.UpdateRun(context.Background(), router.Run{ID: uuid.New(), State: router.StateQueued})
	if err == nil {
		t.Fatal("want error for unknown run")
	}
}

func TestAppendEventUnmarshalablePayloadFailsWithoutRow(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	r := newRun(t, s, c, router.StateRunning, base)
	if err := s.AppendEvent(ctx, c.Tenant, r.ID, "message", make(chan int)); err == nil {
		t.Fatal("want marshal error")
	}
	if evs, err := s.ListEvents(ctx, r.ID); err != nil || len(evs) != 0 {
		t.Fatalf("events = %+v, %v; want none", evs, err)
	}
}

func TestUpdateRunPersistsKind(t *testing.T) {
	s := newStore(t)
	r := newRun(t, s, newConversation(t, s), router.StateClassifying, base)
	r.Kind = "question"
	if err := s.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(context.Background(), r.ID)
	if err != nil || got == nil {
		t.Fatalf("GetRun: %v, %v", got, err)
	}
	if got.Kind != "question" {
		t.Fatalf("kind = %q, want question", got.Kind)
	}
}

func TestHistoryKeepsAgentQuestionBeforeAnswer(t *testing.T) {
	s, ctx := newStore(t), context.Background()
	c := newConversation(t, s)
	run := newRun(t, s, c, router.StateRunning, base)
	add := func(email, body string, fromAgent bool, at time.Time) {
		t.Helper()
		in := router.Input{ID: uuid.New(), Tenant: c.Tenant, ConversationID: c.ID, ActorEmail: email, Body: body,
			Trigger: "assigned", CreatedAt: at, FromAgent: fromAgent}
		if fromAgent {
			in.RunID = &run.ID
		}
		if err := s.AddInput(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	add("dev@x", "mach X", false, base.Add(time.Minute))
	if err := s.ClaimInputs(ctx, c.ID, run.ID); err != nil {
		t.Fatal(err)
	}
	add("", "Welcher Branch?", true, base.Add(2*time.Minute))
	run.State, run.Result, run.UpdatedAt = router.StateWaitingForInput, "Welcher Branch?", base.Add(2*time.Minute)
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	add("dev@x", "main", false, base.Add(3*time.Minute))
	run.State, run.UpdatedAt = router.StateClassifying, base.Add(4*time.Minute) // resumed, result still the question
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenInputs(ctx, c.ID); len(open) != 1 || open[0].Body != "main" {
		t.Fatalf("open = %+v", open)
	}

	hist, err := s.History(ctx, c.ID)

	if err != nil {
		t.Fatal(err)
	}
	want := []router.HistoryEntry{
		{At: base.Add(time.Minute), Author: "dev@x", Text: "mach X"},
		{At: base.Add(2 * time.Minute), Author: "Orchivio", Text: "Welcher Branch?", FromAgent: true},
		{At: base.Add(3 * time.Minute), Author: "dev@x", Text: "main"},
	}
	if !reflect.DeepEqual(hist, want) {
		t.Fatalf("History = %+v\nwant %+v", hist, want)
	}
}
