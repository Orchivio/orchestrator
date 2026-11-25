// Package config holds the per-tenant runtime configuration and keeps it fresh from a Source.
package config

import (
	"time"

	"github.com/google/uuid"
)

// Milestone values a project may report to the tracker.
const (
	MilestoneStarted    = "started"
	MilestoneQuestion   = "question"
	MilestoneClassifier = "classifier"
	MilestoneResult     = "result"
)

// Config holds all tenants keyed by tenant name.
type Config struct{ Tenants map[string]Tenant }

// Tenant credentials are decrypted connection credentials, opaque to the
// orchestrator; they live only in memory and must never be logged.
type Tenant struct {
	Name     string
	Tracker  AdapterRef
	CodeHost AdapterRef
	Profiles map[string]Profile
	Projects []Project
	// TrackerCredential is nil without an active tracker connection.
	TrackerCredential []byte
	// CodeHostCredentials holds one entry per active code host connection.
	CodeHostCredentials [][]byte
	// TrackerConnectionID and CodeHostConnectionIDs name the connections the
	// credentials above came from, index for index.
	TrackerConnectionID   uuid.UUID
	CodeHostConnectionIDs []uuid.UUID
}

type AdapterRef struct {
	Name    string
	Address string
}

type Profile struct {
	Runtime      string
	Model        string
	Instructions string
	MaxDuration  time.Duration
}

type Project struct {
	ID           uuid.UUID
	Name         string
	TrackerKey   string
	Profile      string
	Repos        []Repo
	Classifier   ClassifierRules
	Roles        Roles
	MaxDiffLines int
	Milestones   []string
	// StatusMap maps run states (see StatusMapStates) to tracker status names;
	// states without an entry leave the ticket status alone.
	StatusMap map[string]string
	// ReviewTrigger is ReviewTriggerMention or ReviewTriggerAny; "" means mention.
	ReviewTrigger string
}

// Values of Project.ReviewTrigger: which merge request comments start a run.
const (
	ReviewTriggerMention = "mention"
	ReviewTriggerAny     = "any"
)

// EffectiveReviewTrigger returns ReviewTrigger with the default applied.
func (p Project) EffectiveReviewTrigger() string {
	if p.ReviewTrigger == "" {
		return ReviewTriggerMention
	}
	return p.ReviewTrigger
}

// StatusMapStates lists the run states a StatusMap may key on. Kept in step
// with the router's state constants by a router test (router imports config).
func StatusMapStates() []string {
	return []string{"running", "waiting_for_input", "pushing", "succeeded", "failed", "cancelled"}
}

// Repo.AllowedPaths are path prefixes ("" = everything). Users empty = all
// users with the project role.
type Repo struct {
	Slug         string   `json:"slug"`
	BaseBranch   string   `json:"baseBranch"`
	AllowedPaths []string `json:"allowedPaths"`
	Users        []string `json:"users"`
}

type ClassifierRules struct {
	TaskLabels     []string          `json:"taskLabels"`
	QuestionLabels []string          `json:"questionLabels"`
	RepoLabels     map[string]string `json:"repoLabels"`
	DefaultRepo    string            `json:"defaultRepo"`
}

// Roles lists identities: e-mail addresses or "<system>:<id>" (e.g. "acme:jdoe").
type Roles struct {
	Questions []string `json:"questions"`
	Tasks     []string `json:"tasks"`
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
