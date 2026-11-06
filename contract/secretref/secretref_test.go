package secretref

import (
	"errors"
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
