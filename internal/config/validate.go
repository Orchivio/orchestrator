package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

var (
	roleSystemPattern = regexp.MustCompile(`^[a-z]+:.+$`)
	roleEmailPattern  = regexp.MustCompile(`^[^@\s]+@[^@\s]+$`)
)
var trackerKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// FieldError names the invalid field (JSON path notation) and why.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// DefaultMilestones lists all milestones; the caller fills empty projects with it.
func DefaultMilestones() []string {
	return []string{MilestoneStarted, MilestoneQuestion, MilestoneClassifier, MilestoneResult}
}

// ValidateProject checks a project against its tenant. Tracker-key uniqueness
// is the database's job.
func ValidateProject(t Tenant, p Project) []FieldError {
	var errs []FieldError
	add := func(field, format string, args ...any) {
		errs = append(errs, FieldError{field, fmt.Sprintf(format, args...)})
	}
	if p.Name == "" {
		add("name", "required")
	}
	if p.TrackerKey == "" {
		add("trackerKey", "required")
	} else if !trackerKeyPattern.MatchString(p.TrackerKey) {
		add("trackerKey", "must be uppercase letters and digits, starting with a letter")
	}
	if _, ok := t.Profiles[p.Profile]; !ok {
		add("profile", "unknown profile %q", p.Profile)
	}
	inRepos := func(slug string) bool {
		return slices.ContainsFunc(p.Repos, func(r Repo) bool { return r.Slug == slug })
	}
	if d := p.Classifier.DefaultRepo; d != "" && !inRepos(d) {
		add("classifier.defaultRepo", "%q is not in repos", d)
	}
	labels := make([]string, 0, len(p.Classifier.RepoLabels))
	for label := range p.Classifier.RepoLabels {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		if slug := p.Classifier.RepoLabels[label]; !inRepos(slug) {
			add("classifier.repoLabels."+label, "%q is not in repos", slug)
		}
	}
	known := DefaultMilestones()
	for i, m := range p.Milestones {
		if !slices.Contains(known, m) {
			add(fmt.Sprintf("milestones[%d]", i), "unknown milestone %q", m)
		}
	}
	validateStatusMap(p, add)
	if rt := p.EffectiveReviewTrigger(); rt != ReviewTriggerMention && rt != ReviewTriggerAny {
		add("reviewTrigger", "must be %q or %q", ReviewTriggerMention, ReviewTriggerAny)
	}
	for _, list := range []struct {
		field   string
		entries []string
	}{{"roles.questions", p.Roles.Questions}, {"roles.tasks", p.Roles.Tasks}} {
		for i, e := range list.entries {
			if !roleEmailPattern.MatchString(e) && !roleSystemPattern.MatchString(e) {
				add(fmt.Sprintf("%s[%d]", list.field, i), "%q is neither an e-mail address nor <system>:<id>", e)
			}
		}
	}
	return errs
}

func validateStatusMap(p Project, add func(field, format string, args ...any)) {
	states := StatusMapStates()
	keys := make([]string, 0, len(p.StatusMap))
	for state := range p.StatusMap {
		keys = append(keys, state)
	}
	sort.Strings(keys)
	for _, state := range keys {
		status := p.StatusMap[state]
		switch {
		case !slices.Contains(states, state):
			add("statusMap."+state, "unknown state %q", state)
		case strings.TrimSpace(status) == "":
			add("statusMap."+state, "status name must not be empty")
		}
	}
}
