// Package config loads and validates the per-tenant GitOps configuration.
package config

import "time"

// Milestone values a project may report to the tracker.
const (
	MilestoneStarted    = "started"
	MilestoneQuestion   = "question"
	MilestoneClassifier = "classifier"
	MilestoneResult     = "result"
)

// Config holds all tenants keyed by tenant name.
type Config struct{ Tenants map[string]Tenant }

type Tenant struct {
	Name     string             `yaml:"tenant"`
	Tracker  AdapterRef         `yaml:"tracker"`
	CodeHost AdapterRef         `yaml:"codeHost"`
	Profiles map[string]Profile `yaml:"-"` // decoded via fileDoc (duration parsing)
	Projects []Project          `yaml:"projects"`
}

type AdapterRef struct {
	Name    string `yaml:"name"`
	Address string `yaml:"address"`
}

type Profile struct {
	Runtime      string
	Model        string
	Instructions string
	MaxDuration  time.Duration
}

type Project struct {
	Name         string          `yaml:"name"`
	TrackerKey   string          `yaml:"trackerKey"`
	Profile      string          `yaml:"profile"`
	Repos        []Repo          `yaml:"repos"`
	Classifier   ClassifierRules `yaml:"classifier"`
	Roles        Roles           `yaml:"roles"`
	MaxDiffLines int             `yaml:"maxDiffLines"`
	Milestones   []string        `yaml:"milestones"`
}

// Repo.AllowedPaths are path prefixes ("" = everything). Users empty = all
// users with the project role.
type Repo struct {
	Slug         string   `yaml:"slug"`
	BaseBranch   string   `yaml:"baseBranch"`
	AllowedPaths []string `yaml:"allowedPaths"`
	Users        []string `yaml:"users"`
}

type ClassifierRules struct {
	TaskLabels     []string          `yaml:"taskLabels"`
	QuestionLabels []string          `yaml:"questionLabels"`
	RepoLabels     map[string]string `yaml:"repoLabels"`
	DefaultRepo    string            `yaml:"defaultRepo"`
}

// Roles lists e-mail addresses.
type Roles struct {
	Questions []string `yaml:"questions"`
	Tasks     []string `yaml:"tasks"`
}

// ProjectFor finds the project of a tenant by its tracker key.
func (c *Config) ProjectFor(tenant, trackerKey string) (Tenant, Project, bool) {
	t, ok := c.Tenants[tenant]
	if !ok {
		return Tenant{}, Project{}, false
	}
	for _, p := range t.Projects {
		if p.TrackerKey == trackerKey {
			return t, p, true
		}
	}
	return Tenant{}, Project{}, false
}
