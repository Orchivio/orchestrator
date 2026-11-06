package classifier

import (
	"reflect"
	"strings"
	"testing"

	"github.com/soemii/orchivio-server/internal/config"
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
		{"assigned default task", fixture(), Input{Trigger: "assigned", ActorEmail: "dev@x"}, "task", []string{"A"}, "", false},
		{"mentioned default question", fixture(), Input{Trigger: "mentioned", ActorEmail: "dev@x"}, "question", []string{"A"}, "", false},
		{"task label without role", fixture(), Input{Trigger: "mentioned", Labels: []string{"orchivio:aufgabe"}, ActorEmail: "q@x"}, "question", []string{"A"}, "herabgestuft", false},
		{"question label on assign", fixture(), Input{Trigger: "assigned", Labels: []string{"orchivio:frage"}, ActorEmail: "dev@x"}, "question", []string{"A"}, "", false},
		{"both labels question", fixture(), Input{Trigger: "assigned", Labels: []string{"orchivio:aufgabe", "orchivio:frage"}, ActorEmail: "dev@x"}, "question", []string{"A"}, "", false},
		{"followup keeps task", fixture(), Input{Trigger: "followup", PreviousKind: "task", ActorEmail: "dev@x"}, "task", []string{"A"}, "", false},
		{"followup other actor lacks tasks role", fixture(), Input{Trigger: "followup", PreviousKind: "task", ActorEmail: "dev@x", OtherActors: []string{"q@x"}}, "question", []string{"A"}, "herabgestuft", false},
		{"followup other actors with tasks role", fixture(), Input{Trigger: "followup", PreviousKind: "task", ActorEmail: "dev@x", OtherActors: []string{"pm@x"}}, "task", []string{"A"}, "", false},
		{"followup empty previous", fixture(), Input{Trigger: "followup", ActorEmail: "dev@x"}, "question", []string{"A"}, "", false},
		{"repo dropped no default", fixture(), Input{Trigger: "mentioned", Labels: []string{"repo:b"}, ActorEmail: "pm@x"}, "question", nil, "Berechtigung fehlt für mindestens ein Repo.", true},
		{"no label no default", noDefault, Input{Trigger: "mentioned", ActorEmail: "dev@x"}, "question", nil, "", true},
		{"default filtered by users", func() config.Project {
			p := fixture()
			p.Classifier.DefaultRepo = "B"
			return p
		}(), Input{Trigger: "mentioned", ActorEmail: "pm@x"}, "question", nil, "Berechtigung fehlt für mindestens ein Repo.", true},
		{"two repos in order", fixture(), Input{Trigger: "assigned", Labels: []string{"repo:a", "repo:b", "repo:a"}, ActorEmail: "DEV@x"}, "task", []string{"A", "B"}, "", false},
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
		if got := HasAnyRole(p, email); got != want {
			t.Errorf("%s: got %v", email, got)
		}
	}
}
