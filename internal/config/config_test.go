package config

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadValid(t *testing.T) {
	cfg, err := Load("testdata/valid")
	if err != nil {
		t.Fatal(err)
	}
	tenant, project, ok := cfg.ProjectFor("acme", "ORC")
	if !ok || project.Name != "demo" || tenant.Name != "acme" {
		t.Fatalf("ProjectFor = %v %v %v", tenant.Name, project.Name, ok)
	}
	if want := []string{"started", "question", "classifier", "result"}; !reflect.DeepEqual(project.Milestones, want) {
		t.Fatalf("milestones = %v", project.Milestones)
	}
	if d := tenant.Profiles["default"].MaxDuration; d != 20*time.Minute {
		t.Fatalf("maxDuration = %v", d)
	}
	if _, _, ok := cfg.ProjectFor("acme", "NOPE"); ok {
		t.Fatal("unknown key must not match")
	}
}

func TestDevExampleLoads(t *testing.T) {
	if _, err := Load("../../dev/config"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct{ file, project string }{
		{"unknown-field.yaml", "unknown-field.yaml"},
		{"missing-profile.yaml", "demo"},
		{"default-repo-not-in-repos.yaml", "demo"},
		{"repolabel-not-in-repos.yaml", "demo"},
		{"unknown-milestone.yaml", "demo"},
		{"duplicate-tracker-key.yaml", "dup"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			dir := t.TempDir()
			data, err := os.ReadFile(filepath.Join("testdata/invalid", c.file))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, c.file), data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = Load(dir)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.file) || !strings.Contains(err.Error(), c.project) {
				t.Fatalf("error %q must name file %q and project %q", err, c.file, c.project)
			}
		})
	}
}

func TestLoadRejectsDuplicateTenant(t *testing.T) {
	dir := t.TempDir()
	data, _ := os.ReadFile("testdata/valid/acme.yaml")
	for _, n := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, n), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "acme") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncKeepsLastValid(t *testing.T) {
	dir := t.TempDir()
	data, _ := os.ReadFile("testdata/valid/acme.yaml")
	path := filepath.Join(dir, "acme.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewSync(dir, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	before := s.Current()
	if err := os.WriteFile(path, []byte("tenant: acme\nbogus: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.reload()
	if s.Current() != before {
		t.Fatal("current changed after invalid reload")
	}
	if s.LastError() == nil {
		t.Fatal("LastError must be set")
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s.reload()
	if s.LastError() != nil || s.Current() == before {
		t.Fatal("valid reload must replace config and clear error")
	}
}
