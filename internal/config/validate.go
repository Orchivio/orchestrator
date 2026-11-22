package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
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
	return errs
}
