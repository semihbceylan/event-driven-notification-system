// Package api embeds the OpenAPI specification so the server can serve it.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte
