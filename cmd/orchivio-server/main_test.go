package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

const validKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func lookup(vars map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := vars[k]; return v, ok }
}

func TestOpenBoxRequiresSecretKey(t *testing.T) {
	if _, err := openBox(lookup(nil)); err == nil || !strings.Contains(err.Error(), "ORCHIVIO_SECRET_KEY") {
		t.Fatalf("missing key: err = %v", err)
	}
	const leaked = "not-a-key-but-secret"
	if _, err := openBox(lookup(map[string]string{"ORCHIVIO_SECRET_KEY": leaked})); err == nil || strings.Contains(err.Error(), leaked) {
		t.Fatalf("invalid key: err = %v", err)
	}
	if _, err := openBox(lookup(map[string]string{"ORCHIVIO_SECRET_KEY_REF": leaked})); err == nil || strings.Contains(err.Error(), leaked) {
		t.Fatalf("raw value in ref: err = %v", err)
	}
	if b, err := openBox(lookup(map[string]string{"ORCHIVIO_SECRET_KEY_REF": "env://OTHER", "OTHER": validKey})); err != nil || b == nil {
		t.Fatalf("ref: err = %v", err)
	}
}

func TestOpenRunsRouteIsGone(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(nil, slog.Default()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+uuid.NewString(), nil))
	if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "ticketRef") {
		t.Errorf("code = %d, body %s", rec.Code, rec.Body)
	}
}
