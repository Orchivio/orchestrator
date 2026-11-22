package config

import (
	"slices"
	"testing"
)

func TestProjectForFindsByTrackerKey(t *testing.T) {
	cfg := &Config{Tenants: map[string]Tenant{"acme": {Name: "acme", Projects: []Project{{Name: "demo", TrackerKey: "ORC"}}}}}
	tenant, project, ok := cfg.ProjectFor("acme", "ORC")
	if !ok || project.Name != "demo" || tenant.Name != "acme" {
		t.Fatalf("ProjectFor = %v %v %v", tenant.Name, project.Name, ok)
	}
	if _, _, ok := cfg.ProjectFor("acme", "NOPE"); ok {
		t.Fatal("unknown key must not match")
	}
}

func TestValidateProjectReportsFields(t *testing.T) {
	tn := Tenant{Name: "acme", Profiles: map[string]Profile{"default": {}}}
	p := Project{Name: "demo", TrackerKey: "ORC", Profile: "missing",
		Repos: []Repo{{Slug: "o/r"}}, Classifier: ClassifierRules{DefaultRepo: "o/x", RepoLabels: map[string]string{"repo:y": "o/y"}},
		Milestones: []string{"started", "bogus"}}
	got := ValidateProject(tn, p)
	want := []FieldError{
		{"profile", `unknown profile "missing"`},
		{"classifier.defaultRepo", `"o/x" is not in repos`},
		{"classifier.repoLabels.repo:y", `"o/y" is not in repos`},
		{"milestones[1]", `unknown milestone "bogus"`},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestValidateProjectRequiresNameAndKey(t *testing.T) {
	got := ValidateProject(Tenant{Profiles: map[string]Profile{"d": {}}}, Project{Profile: "d"})
	if !slices.Equal(got, []FieldError{{"name", "required"}, {"trackerKey", "required"}}) {
		t.Fatalf("got %v", got)
	}
}

func TestParseAdapters(t *testing.T) {
	got, err := ParseAdapters("linear=adapter-linear:7101,github=adapter-github:7201")
	if err != nil || got["linear"] != "adapter-linear:7101" || got["github"] != "adapter-github:7201" || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range []string{"", "linear", "linear=", "=a:1", "a=x:1,a=y:2", "a=x:1,,b=y:2"} {
		if _, err := ParseAdapters(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestValidateProjectChecksTrackerKeyFormat(t *testing.T) {
	tn := Tenant{Profiles: map[string]Profile{"d": {}}}
	want := []FieldError{{"trackerKey", "must be uppercase letters and digits, starting with a letter"}}
	for _, bad := range []string{"orc", "1ORC", "OR-C", "ABCDEFGHIJK", " ORC"} {
		if got := ValidateProject(tn, Project{Name: "n", TrackerKey: bad, Profile: "d"}); !slices.Equal(got, want) {
			t.Errorf("%q: got %v", bad, got)
		}
	}
	for _, ok := range []string{"O", "ORC", "ORC2", "ABCDEFGHIJ"} {
		if got := ValidateProject(tn, Project{Name: "n", TrackerKey: ok, Profile: "d"}); len(got) != 0 {
			t.Errorf("%q: got %v", ok, got)
		}
	}
}
