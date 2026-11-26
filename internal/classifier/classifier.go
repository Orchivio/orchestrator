// Package classifier decides with fixed rules (no model) whether a run is a
// question or a task and which repos it gets.
package classifier

import (
	"slices"
	"strings"

	"github.com/orchivio/orchestrator/internal/config"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

type Trigger string

const (
	TriggerAssigned  Trigger = "assigned"
	TriggerMentioned Trigger = "mentioned"
	TriggerFollowup  Trigger = "followup"
)

const (
	KindQuestion = "question"
	KindTask     = "task"

	NoteDowngraded = "Auf Frage herabgestuft: Rolle „Aufgaben“ fehlt."
	NoteRepoDenied = "Berechtigung fehlt für mindestens ein Repo."
)

type Input struct {
	Trigger      Trigger
	PreviousKind string
	Labels       []string
	// Actors are all identities of the acting user (see Identities).
	Actors []string
	// OtherActors wrote further inputs feeding this run, the identities of each
	// actor as one list; a task needs the tasks role of every actor through any
	// of its identities (fail closed), repos stay decided by Actors.
	OtherActors [][]string
}

type Decision struct {
	Kind      string
	Repos     []string
	Notes     []string // German sentences for the ticket
	Reason    []string // internal traceability for run.classified
	NeedsRepo bool
}

// Classify applies the kind rules, then the repo rules of the spec (§6.3, §7.2).
func Classify(p config.Project, in Input) Decision {
	d := Decision{}
	d.Kind = classifyKind(p, in, &d)
	if d.Kind == KindTask && (!hasRole(p.Roles.Tasks, in.Actors) || !allContain(p.Roles.Tasks, in.OtherActors)) {
		d.Kind = KindQuestion
		d.Notes = append(d.Notes, NoteDowngraded)
		d.Reason = append(d.Reason, "downgraded to question: actor lacks tasks role")
	}
	selectRepos(p, in, &d)
	return d
}

func classifyKind(p config.Project, in Input, d *Decision) string {
	c := p.Classifier
	hasTask := hasAny(in.Labels, c.TaskLabels)
	hasQuestion := hasAny(in.Labels, c.QuestionLabels)
	switch {
	case hasTask && hasQuestion:
		d.Reason = append(d.Reason, "task and question labels both present: question (safer)")
		return KindQuestion
	case hasTask:
		d.Reason = append(d.Reason, "task label present")
		return KindTask
	case hasQuestion:
		d.Reason = append(d.Reason, "question label present")
		return KindQuestion
	}
	switch in.Trigger {
	case TriggerAssigned:
		d.Reason = append(d.Reason, "default by trigger assigned: task")
		return KindTask
	case TriggerFollowup:
		if in.PreviousKind != "" {
			d.Reason = append(d.Reason, "followup: kind of previous run ("+in.PreviousKind+")")
			return in.PreviousKind
		}
		d.Reason = append(d.Reason, "followup without previous kind: question")
		return KindQuestion
	}
	d.Reason = append(d.Reason, "default by trigger mentioned: question")
	return KindQuestion
}

func selectRepos(p config.Project, in Input, d *Decision) {
	var candidates []string
	for _, l := range in.Labels {
		slug, ok := lookupFold(p.Classifier.RepoLabels, l)
		if !ok {
			continue
		}
		if !slices.Contains(candidates, slug) {
			candidates = append(candidates, slug)
		}
		d.Reason = append(d.Reason, "repo from label "+l)
	}
	if len(candidates) == 0 && p.Classifier.DefaultRepo != "" {
		candidates = []string{p.Classifier.DefaultRepo}
		d.Reason = append(d.Reason, "no repo label: default repo "+p.Classifier.DefaultRepo)
	}
	denied := false
	for _, slug := range candidates {
		if repoAllows(p, slug, in.Actors) {
			d.Repos = append(d.Repos, slug)
			continue
		}
		denied = true
		d.Reason = append(d.Reason, "repo "+slug+" dropped: actor not permitted")
	}
	if denied {
		d.Notes = append(d.Notes, NoteRepoDenied)
	}
	if len(d.Repos) == 0 {
		d.NeedsRepo = true
		d.Repos = nil
		d.Reason = append(d.Reason, "no repo determinable")
	}
}

func repoAllows(p config.Project, slug string, actors []string) bool {
	for _, r := range p.Repos {
		if r.Slug == slug {
			return len(r.Users) == 0 || hasRole(r.Users, actors)
		}
	}
	return false // unknown slug in config
}

// HasAnyRole reports whether one of the identities holds the questions or tasks role.
func HasAnyRole(p config.Project, actors []string) bool {
	return hasRole(p.Roles.Questions, actors) || hasRole(p.Roles.Tasks, actors)
}

// HasTasksRole reports whether one of the identities holds the tasks role.
func HasTasksRole(p config.Project, actors []string) bool { return hasRole(p.Roles.Tasks, actors) }

// Identities lists how role entries can name the actor: the e-mail address
// and system+":"+username, each when set.
func Identities(system string, a *pluginv1.Actor) []string {
	var ids []string
	if a.GetEmail() != "" {
		ids = append(ids, a.GetEmail())
	}
	if a.GetUsername() != "" {
		ids = append(ids, system+":"+a.GetUsername())
	}
	return ids
}

// FirstMatch picks the identity to store for an input: the first one found in
// the tasks role, else in the questions role, else in a repo users list, else
// the first one. The stored identity is all a later run sees of the actor, so
// the strongest permission must survive.
func FirstMatch(p config.Project, actors []string) string {
	var repoUsers []string
	for _, r := range p.Repos {
		repoUsers = append(repoUsers, r.Users...)
	}
	for _, list := range [][]string{p.Roles.Tasks, p.Roles.Questions, repoUsers} {
		for _, a := range actors {
			if contains(list, a) {
				return a
			}
		}
	}
	if len(actors) > 0 {
		return actors[0]
	}
	return ""
}

// hasRole reports whether any identity is in list.
func hasRole(list, actors []string) bool {
	return slices.ContainsFunc(actors, func(a string) bool { return contains(list, a) })
}

func contains(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func allContain(list []string, actors [][]string) bool {
	return !slices.ContainsFunc(actors, func(a []string) bool { return !hasRole(list, a) })
}

func hasAny(labels, wanted []string) bool {
	return slices.ContainsFunc(labels, func(l string) bool { return contains(wanted, l) })
}

func lookupFold(m map[string]string, key string) (string, bool) {
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}
