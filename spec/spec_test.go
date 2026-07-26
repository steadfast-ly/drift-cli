package spec

import (
	"encoding/json"
	"testing"
)

// The vendored document is what `internal/api` is generated from. These
// assertions are cheap and catch a truncated or wrong-file vendoring long
// before the generated client fails to compile.
func TestVendoredSpecIsTheDriftContract(t *testing.T) {
	if len(OpenAPI) == 0 {
		t.Fatal("the vendored spec is empty")
	}
	if Title() != "Drift API" {
		t.Fatalf("title = %q", Title())
	}
	if OpenAPIVersion() != "3.1.1" {
		t.Fatalf("openapi dialect = %q, want 3.1.1", OpenAPIVersion())
	}
	if APIVersion() == "" {
		t.Fatal("info.version is empty")
	}
}

// The CLI depends on exactly these operations existing. A server that drops one
// is a breaking change the conformance step must catch, not a runtime 404.
func TestSpecDeclaresTheOperationsV1Uses(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
		Components struct {
			SecuritySchemes map[string]any `json:"securitySchemes"`
			Schemas         map[string]any `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(OpenAPI, &doc); err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct{ path, method, op string }{
		{"/environments", "get", "environments.list"},
		{"/environments/{ref}", "get", "environments.get"},
	} {
		ops, ok := doc.Paths[want.path]
		if !ok {
			t.Fatalf("the spec no longer declares %s", want.path)
		}
		if ops[want.method].OperationID != want.op {
			t.Fatalf("%s %s operationId = %q, want %q",
				want.method, want.path, ops[want.method].OperationID, want.op)
		}
	}

	// The bearer scheme is what makes a non-browser client possible at all.
	if _, ok := doc.Components.SecuritySchemes["bearerAuth"]; !ok {
		t.Fatal("the spec no longer declares bearerAuth")
	}
	// One shared error envelope is the premise the whole error-handling design
	// rests on.
	if _, ok := doc.Components.Schemas["ApiProblem"]; !ok {
		t.Fatal("the spec no longer declares ApiProblem")
	}
}
