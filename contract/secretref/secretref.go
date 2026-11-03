// Package secretref resolves secret references such as env://NAME to values.
package secretref

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrNotFound          = errors.New("secret not found")
	ErrUnsupportedScheme = errors.New("unsupported secret scheme")
)

// Provider resolves a secret reference to its value.
type Provider interface {
	Resolve(ref string) (string, error)
}

// Env resolves env://NAME via Lookup (production: os.LookupEnv).
type Env struct {
	Lookup func(string) (string, bool)
}

const envScheme = "env://"

func (e Env) Resolve(ref string) (string, error) {
	name, ok := strings.CutPrefix(ref, envScheme)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedScheme, ref)
	}
	v, found := e.Lookup(name)
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return v, nil
}
