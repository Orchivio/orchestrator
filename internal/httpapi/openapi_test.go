package httpapi_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/orchivio/orchestrator/internal/httpapi"
)

func TestOpenAPIMatchesRoutes(t *testing.T) {
	raw, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	var inSpec []httpapi.Route
	for path, ops := range spec.Paths {
		for method := range ops {
			if method == "parameters" {
				continue
			}
			m := strings.ToUpper(method)
			inSpec = append(inSpec, httpapi.Route{Method: m, Path: path})
			url := strings.NewReplacer("{slug}", "acme", "{id}", uuid.NewString()).Replace(path)
			rec := e.do(m, "/api/v1"+url, "{}")
			if rec.Code == 404 && rec.Header().Get("Content-Type") != "application/problem+json" || rec.Code == 405 {
				t.Errorf("%s %s not routed: %d", m, path, rec.Code)
			}
		}
	}
	for _, r := range httpapi.AdminRoutes {
		if !slices.Contains(inSpec, r) {
			t.Errorf("%s %s missing in spec", r.Method, r.Path)
		}
	}
	if len(inSpec) != len(httpapi.AdminRoutes) {
		t.Errorf("spec has %d operations, routes %d", len(inSpec), len(httpapi.AdminRoutes))
	}
}
