package secretref

import (
	"errors"
	"strings"
	"testing"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestEnvResolve(t *testing.T) {
	p := Env{Lookup: lookupFrom(map[string]string{"LINEAR_TOKEN": "value-1"})}
	got, err := p.Resolve("env://LINEAR_TOKEN")
	if err != nil || got != "value-1" {
		t.Errorf("Resolve() = %q, %v", got, err)
	}
}

func TestEnvResolveMissing(t *testing.T) {
	p := Env{Lookup: lookupFrom(nil)}
	if _, err := p.Resolve("env://NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestEnvResolveRejectsOtherScheme(t *testing.T) {
	p := Env{Lookup: lookupFrom(nil)}
	if _, err := p.Resolve("vault://x"); !errors.Is(err, ErrUnsupportedScheme) {
		t.Errorf("err = %v", err)
	}
}

func TestResolveErrorsNeverEchoRawValue(t *testing.T) {
	p := Env{Lookup: lookupFrom(nil)}
	tests := []struct {
		ref, want string
		target    error
	}{
		{"sk-ant-raw-secret", "unsupported secret scheme", ErrUnsupportedScheme},
		{"vault://sk-ant-raw-secret", "unsupported secret scheme: vault", ErrUnsupportedScheme},
		{"sk-ant-raw-secret://x", "unsupported secret scheme", ErrUnsupportedScheme},
		{"env://sk-ant-raw-secret", "secret not found: malformed env reference", ErrNotFound},
		{"env://MISSING_NAME", "secret not found: MISSING_NAME", ErrNotFound},
	}
	for _, tc := range tests {
		_, err := p.Resolve(tc.ref)
		if !errors.Is(err, tc.target) || err.Error() != tc.want {
			t.Errorf("Resolve(%q) err = %v, want %q", tc.ref, err, tc.want)
		}
	}
}

func TestResolveFileTrimsTrailingNewline(t *testing.T) {
	p := Env{ReadFile: func(string) ([]byte, error) { return []byte("abc\r\n"), nil }}
	got, err := p.Resolve("file:///run/secrets/x")
	if err != nil || got != "abc" {
		t.Errorf("Resolve() = %q, %v", got, err)
	}
}

func TestResolveFileRejectsRelativePath(t *testing.T) {
	p := Env{ReadFile: func(string) ([]byte, error) { return []byte("abc"), nil }}
	if _, err := p.Resolve("file://rel/x"); err == nil {
		t.Error("expected error")
	}
}

func TestResolveFileErrorHidesContent(t *testing.T) {
	p := Env{ReadFile: func(string) ([]byte, error) { return []byte("topsecret"), errors.New("boom") }}
	_, err := p.Resolve("file:///run/secrets/x")
	if err == nil || !strings.Contains(err.Error(), "/run/secrets/x") || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("err = %v", err)
	}
}
