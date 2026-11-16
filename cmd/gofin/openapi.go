package main

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// The API description is generated from the routes as they are registered,
// not written by hand: a hand-maintained spec is out of date the first time
// someone adds an endpoint and forgets it. The cost is that it only knows what
// the mux knows -- method, path, path parameters and whether s.authenticate
// guards the route -- and nothing about query parameters or body shapes.

// authedHandler is what s.authenticate returns. It is a distinct type, rather
// than a plain http.HandlerFunc, so apiMux can tell a guarded route from an
// open one when it records it.
type authedHandler struct {
	admin bool
	h     http.HandlerFunc
}

func (a authedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.h(w, r) }

type authLevel int

const (
	authNone authLevel = iota
	authUser
	authAdmin
)

type apiRoute struct {
	method, path string
	auth         authLevel
}

// apiMux is a ServeMux that remembers every method-qualified pattern it is
// given, for the OpenAPI document.
type apiMux struct {
	*http.ServeMux
	routes []apiRoute
}

func newAPIMux() *apiMux { return &apiMux{ServeMux: http.NewServeMux()} }

func (m *apiMux) HandleFunc(pattern string, h func(http.ResponseWriter, *http.Request)) {
	m.record(pattern, authNone)
	m.ServeMux.HandleFunc(pattern, h)
}

func (m *apiMux) Handle(pattern string, h http.Handler) {
	level := authNone
	if a, ok := h.(authedHandler); ok {
		level = authUser
		if a.admin {
			level = authAdmin
		}
	}
	m.record(pattern, level)
	m.ServeMux.Handle(pattern, h)
}

func (m *apiMux) record(pattern string, level authLevel) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		// A method-less pattern is the catch-all, not an endpoint.
		return
	}
	m.routes = append(m.routes, apiRoute{method: method, path: path, auth: level})
}

type openAPIParam struct {
	Name     string            `json:"name"`
	In       string            `json:"in"`
	Required bool              `json:"required"`
	Schema   map[string]string `json:"schema"`
}

type openAPIOp struct {
	Tags        []string                     `json:"tags"`
	Summary     string                       `json:"summary"`
	Description string                       `json:"description,omitempty"`
	OperationID string                       `json:"operationId"`
	Parameters  []openAPIParam               `json:"parameters,omitempty"`
	Security    []map[string][]string        `json:"security"`
	Responses   map[string]map[string]string `json:"responses"`
}

// Go's {name...} wildcard is a plain {name} as far as OpenAPI is concerned.
var pathWildcard = regexp.MustCompile(`\{([^}.]+)(\.\.\.)?\}`)

// buildOpenAPI renders routes as an OpenAPI 3.0 document. base is the
// configured base URL, so "Try it out" works behind a reverse-proxy prefix.
func buildOpenAPI(routes []apiRoute, base string) ([]byte, error) {
	authed := []map[string][]string{{"TokenHeader": {}}, {"ApiKeyQuery": {}}}
	paths := map[string]map[string]openAPIOp{}
	for _, rt := range routes {
		path := pathWildcard.ReplaceAllString(rt.path, "{$1}")
		op := openAPIOp{
			Tags:        []string{tagFor(path)},
			Summary:     rt.method + " " + path,
			OperationID: operationID(rt.method, path),
			Security:    []map[string][]string{}, // explicitly open
			Responses:   map[string]map[string]string{"default": {"description": "Response"}},
		}
		for _, m := range pathWildcard.FindAllStringSubmatch(path, -1) {
			op.Parameters = append(op.Parameters, openAPIParam{
				Name: m[1], In: "path", Required: true, Schema: map[string]string{"type": "string"},
			})
		}
		switch rt.auth {
		case authUser:
			op.Security = authed
			op.Responses["401"] = map[string]string{"description": "Missing or invalid token"}
		case authAdmin:
			op.Security = authed
			op.Description = "Requires an administrator token."
			op.Responses["401"] = map[string]string{"description": "Missing or invalid token"}
			op.Responses["403"] = map[string]string{"description": "Not an administrator"}
		}
		if paths[path] == nil {
			paths[path] = map[string]openAPIOp{}
		}
		paths[path][strings.ToLower(rt.method)] = op
	}

	server := base
	if server == "" {
		server = "/"
	}
	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "gofin",
			"version": Version,
			"description": "Jellyfin-compatible API served by gofin. Generated from the " +
				"registered routes: methods, paths and authentication are exact, but " +
				"query parameters and request/response bodies are not described.",
		},
		"servers": []map[string]string{{"url": server}},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"TokenHeader": map[string]string{
					"type": "apiKey", "in": "header", "name": "X-Emby-Token",
					"description": "An access token from POST /Users/AuthenticateByName.",
				},
				"ApiKeyQuery": map[string]string{
					"type": "apiKey", "in": "query", "name": "api_key",
				},
			},
		},
		"paths": paths,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// tagFor groups operations by their first path segment, skipping the legacy
// /Users/{userId} prefix so /Users/{userId}/Items sits with /Items.
func tagFor(path string) string {
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if len(segs) > 2 && segs[0] == "Users" && segs[1] == "{userId}" {
		segs = segs[2:]
	}
	return segs[0]
}

func operationID(method, path string) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(path, "/") {
		seg = strings.Trim(seg, "{}")
		if seg == "" {
			continue
		}
		b.WriteString("_")
		b.WriteString(seg)
	}
	return b.String()
}

// swaggerUI loads Swagger UI from a pinned CDN build and points it at the
// spec next to it. The relative URL keeps it working under a base URL.
const swaggerUI = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>gofin API</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui.css">
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
<script>
window.ui = SwaggerUIBundle({
  url: new URL("../openapi.json", location.href.replace(/\/?$/, "/")).href,
  dom_id: "#swagger-ui",
  persistAuthorization: true,
  tryItOutEnabled: true,
});
</script>
</body>
</html>
`

// registerAPIDocs adds the spec and the UI. It must run after every other
// route is registered, since the spec is a snapshot of mux.routes.
func (s *Server) registerAPIDocs(mux *apiMux) {
	var spec []byte
	mux.HandleFunc("GET /api-docs/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(spec)
	})
	ui := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerUI))
	}
	mux.HandleFunc("GET /api-docs/swagger", ui)
	// The trailing-slash spelling is an alias, so it bypasses the recorder
	// rather than appear in the spec as a second operation.
	mux.ServeMux.HandleFunc("GET /api-docs/swagger/{$}", ui)

	var err error
	spec, err = buildOpenAPI(mux.routes, s.cfg.Server.BaseURL)
	if err != nil {
		// Only reachable if the document holds something json cannot encode,
		// which is a programming error, not a runtime condition.
		panic("building OpenAPI document: " + err.Error())
	}
}
