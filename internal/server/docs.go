package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
)

// openAPITemplate is the API's OpenAPI document. Its info.version is the
// placeholder versionPlaceholder, filled in with the build version when served.
//
//go:embed api/openapi.json
var openAPITemplate []byte

//go:embed api/docs.html
var docsHTML []byte

const versionPlaceholder = `"{{VERSION}}"`

// openAPISpec returns the OpenAPI document with info.version set to Version().
var openAPISpec = sync.OnceValue(func() []byte {
	v, _ := json.Marshal(Version()) // marshaling a string cannot fail
	return bytes.Replace(openAPITemplate, []byte(versionPlaceholder), v, 1)
})

// openAPI serves the OpenAPI document.
func openAPI(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	_, err := w.Write(openAPISpec())
	return err
}

// docs serves the Redoc API reference page, which renders /openapi.json.
func docs(w http.ResponseWriter, _ *http.Request) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := w.Write(docsHTML)
	return err
}
