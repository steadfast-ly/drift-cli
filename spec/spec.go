// Package spec vendors the server's OpenAPI document and makes the few facts
// the CLI needs from it available at runtime.
//
// The document is the SOURCE for `internal/api`, which is generated from it and
// never hand-edited. Embedding the same bytes the generator consumed means
// `drift version` reports the contract this binary was actually built against,
// rather than a constant somebody has to remember to bump.
package spec

import (
	_ "embed"
	"encoding/json"
	"sync"
)

// OpenAPI is the vendored document, byte-identical to the server's committed
// `openapi.json`. The CI drift check regenerates `internal/api` from these
// bytes and fails if the result differs from what is committed.
//
//go:embed openapi.json
var OpenAPI []byte

type info struct {
	Info struct {
		Title   string `json:"title"`
		Version string `json:"version"`
	} `json:"info"`
	OpenAPI string `json:"openapi"`
}

var (
	once   sync.Once
	parsed info
)

func load() {
	once.Do(func() { _ = json.Unmarshal(OpenAPI, &parsed) })
}

// APIVersion is the contract version from the document's `info.version`.
func APIVersion() string { load(); return parsed.Info.Version }

// OpenAPIVersion is the OpenAPI dialect the document declares.
func OpenAPIVersion() string { load(); return parsed.OpenAPI }

// Title is the document's title.
func Title() string { load(); return parsed.Info.Title }
