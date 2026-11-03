// Package repofile parses .orchivio.yaml from a repository. A repo file may
// only tighten the server config (extra instructions, forbidden paths); it has
// no field that could widen permissions, and unknown fields are rejected.
package repofile

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// RepoFile is the restrictive per-repository configuration.
type RepoFile struct {
	Instructions   string   `yaml:"instructions"`
	ForbiddenPaths []string `yaml:"forbiddenPaths"`
}

// Parse decodes a .orchivio.yaml; unknown fields (e.g. allowedPaths) are errors.
func Parse(data []byte) (RepoFile, error) {
	var f RepoFile
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) { // EOF = empty file
		return RepoFile{}, fmt.Errorf("parse .orchivio.yaml: %w", err)
	}
	return f, nil
}
