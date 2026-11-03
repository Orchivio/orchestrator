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
