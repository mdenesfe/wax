package api

import (
	_ "embed"
	"net/http"
)

// openAPI is the API contract (OpenAPI 3.1). TestOpenAPIContract keeps it
// in sync with the routes and the JSON shapes of the code.
//
//go:embed openapi.json
var openAPI []byte

func serveOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPI)
}
