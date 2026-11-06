// Package classifier decides with fixed rules (no model) whether a run is a
// question or a task and which repos it gets.
package classifier

import (
	"slices"
	"strings"

	"github.com/soemii/orchivio-server/internal/config"
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
	ActorEmail   string
	// OtherActors wrote further inputs feeding this run; a task needs the tasks role
	// of every one of them (fail closed), repos stay decided by ActorEmail.
	OtherActors []string
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
	if d.Kind == KindTask && !allContain(p.Roles.Tasks, append([]string{in.ActorEmail}, in.OtherActors...)) {
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
		if repoAllows(p, slug, in.ActorEmail) {
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

func repoAllows(p config.Project, slug, email string) bool {
	for _, r := range p.Repos {
		if r.Slug == slug {
			return len(r.Users) == 0 || contains(r.Users, email)
		}
	}
	return false // unknown slug in config
}

// HasAnyRole reports whether the email holds the questions or tasks role.
func HasAnyRole(p config.Project, email string) bool {
	return contains(p.Roles.Questions, email) || contains(p.Roles.Tasks, email)
}

func contains(list []string, s string) bool {
	return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, s) })
}

func allContain(list, emails []string) bool {
	return !slices.ContainsFunc(emails, func(e string) bool { return !contains(list, e) })
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
