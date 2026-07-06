package api

import (
	"testing"

	"github.com/getkin/kin-openapi/routers/gorillamux"

	"github.com/pyaethu-aung/image-server/internal/api/gen"
)

// TestSpecIsValid loads the embedded OpenAPI spec, validates it, and builds
// the validation router the apitest harness uses, so a malformed spec fails
// plain `make test` without needing services.
func TestSpecIsValid(t *testing.T) {
	spec, err := gen.GetSwagger()
	if err != nil {
		t.Fatalf("load embedded OpenAPI spec: %v", err)
	}
	if err := spec.Validate(t.Context()); err != nil {
		t.Fatalf("OpenAPI spec is invalid: %v", err)
	}
	if _, err := gorillamux.NewRouter(spec); err != nil {
		t.Fatalf("spec cannot back the validation router: %v", err)
	}
}
