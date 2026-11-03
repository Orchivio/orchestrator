package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// fileDoc is the on-disk shape of one tenant file; it differs from Tenant only
// in the profile durations, which YAML carries as strings like "20m".
type fileDoc struct {
	Tenant   `yaml:",inline"`
	Profiles map[string]fileProfile `yaml:"profiles"`
}

type fileProfile struct {
	Runtime      string `yaml:"runtime"`
	Model        string `yaml:"model"`
	Instructions string `yaml:"instructions"`
	MaxDuration  string `yaml:"maxDuration"`
}

// Load reads every *.yaml in dir (one tenant per file) and validates it.
func Load(dir string) (*Config, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("config: no *.yaml files in %s", dir)
	}
	slices.Sort(files)
	cfg := &Config{Tenants: map[string]Tenant{}}
	for _, f := range files {
		t, err := loadFile(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(f), err)
		}
		if _, dup := cfg.Tenants[t.Name]; dup {
			return nil, fmt.Errorf("%s: duplicate tenant %q", filepath.Base(f), t.Name)
		}
		cfg.Tenants[t.Name] = t
	}
	return cfg, nil
}

func loadFile(path string) (Tenant, error) {
	f, err := os.Open(path)
	if err != nil {
		return Tenant{}, err
	}
	defer f.Close()
	var doc fileDoc
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		return Tenant{}, err
	}
	t := doc.Tenant
	if t.Name == "" {
		return Tenant{}, fmt.Errorf("tenant name is required")
	}
	t.Profiles = map[string]Profile{}
	for name, p := range doc.Profiles {
		d, err := time.ParseDuration(p.MaxDuration)
		if err != nil {
			return Tenant{}, fmt.Errorf("profile %q: maxDuration: %w", name, err)
		}
		t.Profiles[name] = Profile{p.Runtime, p.Model, p.Instructions, d}
	}
	keys := map[string]bool{}
	for i := range t.Projects {
		p := &t.Projects[i]
		if err := validateProject(t, p, keys); err != nil {
			return Tenant{}, fmt.Errorf("project %q: %w", p.Name, err)
		}
	}
	return t, nil
}

var defaultMilestones = []string{MilestoneStarted, MilestoneQuestion, MilestoneClassifier, MilestoneResult}

func validateProject(t Tenant, p *Project, keys map[string]bool) error {
	if p.Name == "" || p.TrackerKey == "" {
		return fmt.Errorf("name and trackerKey are required")
	}
	if keys[p.TrackerKey] {
		return fmt.Errorf("duplicate trackerKey %q", p.TrackerKey)
	}
	keys[p.TrackerKey] = true
	if _, ok := t.Profiles[p.Profile]; !ok {
		return fmt.Errorf("unknown profile %q", p.Profile)
	}
	slugs := map[string]bool{}
	for _, r := range p.Repos {
		slugs[r.Slug] = true
	}
	if d := p.Classifier.DefaultRepo; d != "" && !slugs[d] {
		return fmt.Errorf("classifier.defaultRepo %q is not in repos", d)
	}
	for label, slug := range p.Classifier.RepoLabels {
		if !slugs[slug] {
			return fmt.Errorf("classifier.repoLabels[%q] target %q is not in repos", label, slug)
		}
	}
	if len(p.Milestones) == 0 {
		p.Milestones = slices.Clone(defaultMilestones)
	}
	for _, m := range p.Milestones {
		if !slices.Contains(defaultMilestones, m) {
			return fmt.Errorf("unknown milestone %q", m)
		}
	}
	return nil
}
