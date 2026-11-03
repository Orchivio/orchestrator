package repofile

import (
	"reflect"
	"testing"
)

func TestParseRepoFileRejectsLoosening(t *testing.T) {
	if _, err := Parse([]byte("allowedPaths: [x]\n")); err == nil {
		t.Fatal("allowedPaths must be rejected")
	}
}

func TestParseRepoFileAcceptsRestrictions(t *testing.T) {
	got, err := Parse([]byte("instructions: be nice\nforbiddenPaths: [secrets/]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := RepoFile{Instructions: "be nice", ForbiddenPaths: []string{"secrets/"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v want %+v", got, want)
	}
}
