// Package secretref resolves secret references such as env://NAME to values.
package secretref

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// Env resolves env://NAME via Lookup (production: os.LookupEnv) and
// file:///abs/path via ReadFile (nil: os.ReadFile).
type Env struct {
	Lookup   func(string) (string, bool)
	ReadFile func(name string) ([]byte, error)
}

const (
	envScheme  = "env://"
	fileScheme = "file://"
)

// Errors echo at most a plain scheme or a well-formed variable name: a raw secret
// pasted into a *_REF variable must never reach a log.
var (
	schemePattern  = regexp.MustCompile(`^[a-z]{1,16}$`)
	envNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (e Env) Resolve(ref string) (string, error) {
	if path, ok := strings.CutPrefix(ref, fileScheme); ok {
		return e.resolveFile(path)
	}
	name, ok := strings.CutPrefix(ref, envScheme)
	if !ok {
		if scheme, _, found := strings.Cut(ref, "://"); found && schemePattern.MatchString(scheme) {
			return "", fmt.Errorf("%w: %s", ErrUnsupportedScheme, scheme)
		}
		return "", ErrUnsupportedScheme
	}
	if !envNamePattern.MatchString(name) {
		return "", fmt.Errorf("%w: malformed env reference", ErrNotFound)
	}
	v, found := e.Lookup(name)
	if !found {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return v, nil
}

func (e Env) resolveFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: file reference must be absolute", ErrNotFound)
	}
	read := e.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	b, err := read(path)
	if err != nil {
		return "", fmt.Errorf("%w: cannot read %s", ErrNotFound, path)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
