package classifier

import (
	"reflect"
	"strings"
	"testing"

	"github.com/orchivio/orchestrator/internal/config"

	pluginv1 "github.com/orchivio/orchestrator/contract/gen/orchivio/plugin/v1"
)

func fixture() config.Project {
	return config.Project{
		Repos: []config.Repo{
			{Slug: "A"},
			{Slug: "B", Users: []string{"dev@x"}},
		},
		Classifier: config.ClassifierRules{
			TaskLabels:     []string{"orchivio:aufgabe"},
			QuestionLabels: []string{"orchivio:frage"},
			RepoLabels:     map[string]string{"repo:a": "A", "repo:b": "B"},
			DefaultRepo:    "A",
		},
		Roles: config.Roles{
			Questions: []string{"q@x", "pm@x"},
			Tasks:     []string{"dev@x", "PM@x"},
		},
	}
}

func TestClassify(t *testing.T) {
	noDefault := fixture()
	noDefault.Classifier.DefaultRepo = ""
	tests := []struct {
		name      string
		p         config.Project
		in        Input
		kind      string
		repos     []string
		note      string
		needsRepo bool
	}{
		{"assigned default task", fixture(), Input{Trigger: "assigned", Actors: []string{"dev@x"}}, "task", []string{"A"}, "", false},
		{"mentioned default question", fixture(), Input{Trigger: "mentioned", Actors: []string{"dev@x"}}, "question", []string{"A"}, "", false},
		{"task label without role", fixture(), Input{Trigger: "mentioned", Labels: []string{"orchivio:aufgabe"}, Actors: []string{"q@x"}}, "question", []string{"A"}, "herabgestuft", false},
		{"question label on assign", fixture(), Input{Trigger: "assigned", Labels: []string{"orchivio:frage"}, Actors: []string{"dev@x"}}, "question", []string{"A"}, "", false},
		{"both labels question", fixture(), Input{Trigger: "assigned", Labels: []string{"orchivio:aufgabe", "orchivio:frage"}, Actors: []string{"dev@x"}}, "question", []string{"A"}, "", false},
		{"followup keeps task", fixture(), Input{Trigger: "followup", PreviousKind: "task", Actors: []string{"dev@x"}}, "task", []string{"A"}, "", false},
		{"followup other actor lacks tasks role", fixture(), Input{Trigger: "followup", PreviousKind: "task", Actors: []string{"dev@x"}, OtherActors: []string{"q@x"}}, "question", []string{"A"}, "herabgestuft", false},
		{"followup other actors with tasks role", fixture(), Input{Trigger: "followup", PreviousKind: "task", Actors: []string{"dev@x"}, OtherActors: []string{"pm@x"}}, "task", []string{"A"}, "", false},
		{"followup empty previous", fixture(), Input{Trigger: "followup", Actors: []string{"dev@x"}}, "question", []string{"A"}, "", false},
		{"repo dropped no default", fixture(), Input{Trigger: "mentioned", Labels: []string{"repo:b"}, Actors: []string{"pm@x"}}, "question", nil, "Berechtigung fehlt für mindestens ein Repo.", true},
		{"no label no default", noDefault, Input{Trigger: "mentioned", Actors: []string{"dev@x"}}, "question", nil, "", true},
		{"default filtered by users", func() config.Project {
			p := fixture()
			p.Classifier.DefaultRepo = "B"
			return p
		}(), Input{Trigger: "mentioned", Actors: []string{"pm@x"}}, "question", nil, "Berechtigung fehlt für mindestens ein Repo.", true},
		{"two repos in order", fixture(), Input{Trigger: "assigned", Labels: []string{"repo:a", "repo:b", "repo:a"}, Actors: []string{"DEV@x"}}, "task", []string{"A", "B"}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Classify(tc.p, tc.in)
			if d.Kind != tc.kind || !reflect.DeepEqual(d.Repos, tc.repos) || d.NeedsRepo != tc.needsRepo {
				t.Fatalf("got %+v", d)
			}
			if tc.note == "" && len(d.Notes) != 0 {
				t.Fatalf("unexpected notes %v", d.Notes)
			}
			if tc.note != "" && !strings.Contains(strings.Join(d.Notes, "|"), tc.note) {
				t.Fatalf("notes %v lack %q", d.Notes, tc.note)
			}
			if len(d.Reason) == 0 {
				t.Fatal("empty reason")
			}
			for _, n := range d.Notes {
				if strings.Contains(n, "B") && strings.Contains(n, "Repo B") {
					t.Fatalf("note leaks repo name: %s", n)
				}
			}
		})
	}
}

func TestHasAnyRole(t *testing.T) {
	p := fixture()
	for email, want := range map[string]bool{"nobody@x": false, "DEV@X": true, "Q@x": true} {
		if got := HasAnyRole(p, []string{email}); got != want {
			t.Errorf("%s: got %v", email, got)
		}
	}
}

func TestRoleMatchesSystemUsername(t *testing.T) {
	p := fixture()
	p.Roles.Tasks = []string{"jira:jdoe"}
	a := &pluginv1.Actor{Id: "5b10", Username: "jdoe"}
	d := Classify(p, Input{Trigger: "assigned", Actors: Identities("jira", a)})
	if d.Kind != "task" {
		t.Fatalf("got %+v", d)
	}
	if !HasAnyRole(p, Identities("jira", a)) {
		t.Fatal("role missing")
	}
	if HasAnyRole(p, Identities("gitlab", a)) {
		t.Fatal("system must be part of the identity")
	}
}

func TestRoleEmailStillMatches(t *testing.T) {
	a := &pluginv1.Actor{Id: "u1", Email: "dev@x"}
	ids := Identities("linear", a)
	if len(ids) != 1 || ids[0] != "dev@x" {
		t.Fatalf("ids = %v", ids)
	}
	if d := Classify(fixture(), Input{Trigger: "assigned", Actors: ids}); d.Kind != "task" {
		t.Fatalf("got %+v", d)
	}
}

func TestIdentitiesListsEmailAndUsername(t *testing.T) {
	got := Identities("jira", &pluginv1.Actor{Email: "a@b.de", Username: "jdoe"})
	if !reflect.DeepEqual(got, []string{"a@b.de", "jira:jdoe"}) {
		t.Fatalf("got %v", got)
	}
}
