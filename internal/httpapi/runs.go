package httpapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/orchivio/orchestrator/internal/admin"
)

// invalidLimit makes the service reject an explicit limit that is not a positive integer.
const invalidLimit = -1

func (a *adminAPI) listRuns(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := admin.RunFilter{Ticket: q.Get("ticket"), State: q.Get("state"), Before: q.Get("before")}
	if raw := q.Get("limit"); raw != "" {
		var err error
		if f.Limit, err = strconv.Atoi(raw); err != nil || f.Limit == 0 {
			f.Limit = invalidLimit
		}
	}
	page, err := a.svc.ListRuns(r.Context(), r.PathValue("slug"), projectID, f)
	if err != nil {
		a.failRuns(w, err)
		return
	}
	out := struct {
		Runs []runJSON `json:"runs"`
		Next string    `json:"next,omitempty"`
	}{Runs: make([]runJSON, len(page.Runs)), Next: page.Next}
	for i, run := range page.Runs {
		out.Runs[i] = toRunJSON(run)
	}
	a.send(w, http.StatusOK, 0, out)
}

func (a *adminAPI) getRun(w http.ResponseWriter, r *http.Request) {
	projectID, ok := pathID(w, r)
	runID, err := uuid.Parse(r.PathValue("runId"))
	if !ok {
		return
	}
	if err != nil {
		a.fail(w, admin.ErrNotFound)
		return
	}
	run, events, err := a.svc.GetRun(r.Context(), r.PathValue("slug"), projectID, runID)
	if err != nil {
		a.fail(w, err)
		return
	}
	out := struct {
		Run    runJSON     `json:"run"`
		Events []eventJSON `json:"events"`
	}{Run: toRunJSON(run), Events: make([]eventJSON, len(events))}
	for i, e := range events {
		out.Events[i] = eventJSON(e)
	}
	a.send(w, http.StatusOK, 0, out)
}

// failRuns answers invalid query parameters with 400 (the list has no body to be unprocessable).
func (a *adminAPI) failRuns(w http.ResponseWriter, err error) {
	var ve *admin.ValidationError
	if errors.As(err, &ve) {
		writeProblemFields(w, http.StatusBadRequest, "Bad Request", "A query parameter is invalid.", ve.Fields)
		return
	}
	a.fail(w, err)
}
