package config

import "testing"

func validProject() (Tenant, Project) {
	return Tenant{Profiles: map[string]Profile{"default": {}}},
		Project{Name: "demo", TrackerKey: "ORC", Profile: "default"}
}

func fieldsOf(errs []FieldError) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Field
	}
	return out
}

func TestStatusMapRejectsUnknownState(t *testing.T) {
	tn, p := validProject()
	p.StatusMap = map[string]string{"done": "Done"}
	if got := fieldsOf(ValidateProject(tn, p)); len(got) != 1 || got[0] != "statusMap.done" {
		t.Fatalf("fields = %v", got)
	}
}

func TestStatusMapRejectsEmptyValue(t *testing.T) {
	tn, p := validProject()
	p.StatusMap = map[string]string{"running": ""}
	if got := fieldsOf(ValidateProject(tn, p)); len(got) != 1 || got[0] != "statusMap.running" {
		t.Fatalf("fields = %v", got)
	}
}

func TestStatusMapRejectsWhitespaceValue(t *testing.T) {
	tn, p := validProject()
	p.StatusMap = map[string]string{"running": " \t"}
	if got := fieldsOf(ValidateProject(tn, p)); len(got) != 1 || got[0] != "statusMap.running" {
		t.Fatalf("fields = %v", got)
	}
}

func TestStatusMapAcceptsRunStates(t *testing.T) {
	tn, p := validProject()
	p.StatusMap = map[string]string{"running": "In Progress", "waiting_for_input": "Blocked", "pushing": "In Progress",
		"succeeded": "In Review", "failed": "Open", "cancelled": "Open"}
	if errs := ValidateProject(tn, p); len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
}

func TestReviewTriggerValues(t *testing.T) {
	tn, p := validProject()
	p.ReviewTrigger = ReviewTriggerAny
	if errs := ValidateProject(tn, p); len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	p.ReviewTrigger = "always"
	if got := fieldsOf(ValidateProject(tn, p)); len(got) != 1 || got[0] != "reviewTrigger" {
		t.Fatalf("fields = %v", got)
	}
}

func TestEffectiveReviewTriggerDefaultsToMention(t *testing.T) {
	if got := (Project{}).EffectiveReviewTrigger(); got != ReviewTriggerMention {
		t.Fatalf("got %q", got)
	}
	if got := (Project{ReviewTrigger: ReviewTriggerAny}).EffectiveReviewTrigger(); got != ReviewTriggerAny {
		t.Fatalf("got %q", got)
	}
}

func TestRoleEntryForms(t *testing.T) {
	for _, ok := range []string{"a@b.de", "jira:jdoe", "jira:5b10ac8d82e05b22cc7d4ef5"} {
		tn, p := validProject()
		p.Roles = Roles{Questions: []string{ok}, Tasks: []string{ok}}
		if errs := ValidateProject(tn, p); len(errs) != 0 {
			t.Errorf("%q rejected: %v", ok, errs)
		}
	}
	for _, bad := range []string{"JIRA:x", "jdoe"} {
		tn, p := validProject()
		p.Roles = Roles{Questions: []string{bad}, Tasks: []string{"a@b.de", bad}}
		got := fieldsOf(ValidateProject(tn, p))
		if len(got) != 2 || got[0] != "roles.questions[0]" || got[1] != "roles.tasks[1]" {
			t.Errorf("%q: fields = %v", bad, got)
		}
	}
}

func TestCommentTriggerValues(t *testing.T) {
	tn, p := validProject()
	p.CommentTrigger = CommentTriggerMention
	if errs := ValidateProject(tn, p); len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	p.CommentTrigger = "always"
	if got := fieldsOf(ValidateProject(tn, p)); len(got) != 1 || got[0] != "commentTrigger" {
		t.Fatalf("fields = %v", got)
	}
	if got := (Project{}).EffectiveCommentTrigger(); got != CommentTriggerAny {
		t.Fatalf("default = %q", got)
	}
}
