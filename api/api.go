// Package api embeds the OpenAPI description served at /openapi.yaml.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document for burnrate's HTTP API.
//
//go:embed openapi.yaml
var OpenAPI []byte
