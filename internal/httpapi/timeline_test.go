package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/soemii/orchivio-server/internal/router"
)

type fakeStore struct {
	run    *router.Run
	events []Event
}

func (f fakeStore) GetRun(context.Context, uuid.UUID) (*router.Run, error) { return f.run, nil }
func (f fakeStore) ListEvents(context.Context, uuid.UUID) ([]Event, error) { return f.events, nil }

func get(h http.Handler, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestTimelineReturnsRunAndEvents(t *testing.T) {
	id := uuid.New()
	at := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	h := NewTimeline(fakeStore{
		run:    &router.Run{ID: id, TicketRef: "ORC-1", State: router.StateSucceeded, PRURLs: []string{"u"}},
		events: []Event{{Type: "message", Payload: json.RawMessage(`{"text":"hi"}`), At: at}},
	})
	rec := get(h, "/runs/"+id.String())
	if rec.Code != 200 {
		t.Fatalf("code = %d", rec.Code)
	}
	var out struct {
		Run    struct{ ID, State, TicketRef string }
		Events []struct {
			Type    string
			Payload struct{ Text string }
			At      time.Time
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Run.ID != id.String() || out.Run.State != "succeeded" || len(out.Events) != 1 ||
		out.Events[0].Type != "message" || out.Events[0].Payload.Text != "hi" || !out.Events[0].At.Equal(at) {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestTimelineNoEventsIsEmptyArray(t *testing.T) {
	id := uuid.New()
	rec := get(NewTimeline(fakeStore{run: &router.Run{ID: id}}), "/runs/"+id.String())
	var out map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if string(out["events"]) != "[]" {
		t.Errorf("body = %s", rec.Body)
	}
}

func TestTimelineUnknownRun404(t *testing.T) {
	if rec := get(NewTimeline(fakeStore{}), "/runs/"+uuid.NewString()); rec.Code != 404 {
		t.Errorf("code = %d", rec.Code)
	}
}

func TestTimelineInvalidID400(t *testing.T) {
	if rec := get(NewTimeline(fakeStore{}), "/runs/nope"); rec.Code != 400 {
		t.Errorf("code = %d", rec.Code)
	}
}
