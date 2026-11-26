package router

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/contract/redact"
	"github.com/orchivio/orchestrator/internal/classifier"
	"github.com/orchivio/orchestrator/internal/config"
)

const (
	eventStatusFailed = "status_failed"
	maxSlugLen        = 40
	// AdapterCallTimeout bounds adapter calls made outside a request deadline,
	// so a hanging adapter cannot block a run transition or a cleanup.
	AdapterCallTimeout = 15 * time.Second
)

var asciiFold = strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss")

// Slug turns a ticket title into a branch-name suffix: ASCII-folded, lower
// case, [a-z0-9] runs joined by "-", at most maxSlugLen long. Empty when
// nothing usable remains.
func Slug(title string) string {
	var b strings.Builder
	pendingDash := false
	for _, c := range asciiFold.Replace(strings.ToLower(title)) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9':
			if pendingDash && b.Len() > 0 {
				b.WriteByte('-')
			}
			pendingDash = false
			b.WriteRune(c)
		case c < unicode.MaxASCII:
			pendingDash = true
		}
	}
	s := b.String()
	if len(s) > maxSlugLen {
		s = s[:maxSlugLen]
	}
	return strings.TrimRight(s, "-")
}

// EventAppender is the part of the stores needed to record status failures.
type EventAppender interface {
	AppendEvent(ctx context.Context, tenant string, runID uuid.UUID, typ string, payload any) error
}

// ApplyStatus sets the ticket status mapped to the run's new state. Only task
// runs set status. It never fails the run: an error is recorded as a
// status_failed event and logged.
func ApplyStatus(ctx context.Context, cfg *config.Sync, tracker Tracker, events EventAppender, run Run, state string) {
	if run.Kind != classifier.KindTask {
		return
	}
	_, project, ok := cfg.Current().ProjectFor(run.Tenant, run.Project)
	if !ok {
		return
	}
	status, ok := project.StatusMap[state]
	if !ok {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, AdapterCallTimeout)
	err := tracker.SetStatus(callCtx, run.Tenant, run.TicketRef, status)
	cancel()
	if err == nil {
		return
	}
	slog.Warn("set ticket status failed", "run", run.ID, "ticket", run.TicketRef, "state", state, "status", status, "err", redact.String(err.Error()))
	payload := map[string]string{"state": state, "status": status, "error": redact.String(err.Error())}
	if aerr := events.AppendEvent(ctx, run.Tenant, run.ID, eventStatusFailed, payload); aerr != nil {
		slog.Error("record status_failed event failed", "run", run.ID, "err", aerr)
	}
}
