package main

import (
	"encoding/json"
	"strings"
	"testing"
)

type specOp struct {
	OperationID string                `json:"operationId"`
	Security    []map[string][]string `json:"security"`
	Parameters  []struct {
		Name string `json:"name"`
		In   string `json:"in"`
	} `json:"parameters"`
	Responses map[string]any `json:"responses"`
}

type spec struct {
	OpenAPI string `json:"openapi"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths map[string]map[string]specOp `json:"paths"`
}

func fetchSpec(t *testing.T, e *testEnv) spec {
	t.Helper()
	w := e.do(t, "GET", "/api-docs/openapi.json", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var s spec
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return s
}

func TestOpenAPIListsRegisteredRoutes(t *testing.T) {
	s := fetchSpec(t, newTestEnv(t))
	if s.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q", s.OpenAPI)
	}

	// Open route: security is present and empty, so Swagger UI sends no token.
	ping, ok := s.Paths["/System/Ping"]["get"]
	if !ok {
		t.Fatal("GET /System/Ping missing")
	}
	if ping.Security == nil || len(ping.Security) != 0 {
		t.Errorf("open route security = %v, want []", ping.Security)
	}
	if _, ok := s.Paths["/System/Ping"]["post"]; !ok {
		t.Error("POST /System/Ping missing: methods on one path must merge")
	}

	// Admin route: guarded, with a 403.
	refresh, ok := s.Paths["/Library/Refresh"]["post"]
	if !ok {
		t.Fatal("POST /Library/Refresh missing")
	}
	if len(refresh.Security) == 0 {
		t.Error("admin route has no security requirement")
	}
	if _, ok := refresh.Responses["403"]; !ok {
		t.Error("admin route should document 403")
	}

	// User route: guarded, but no 403.
	items, ok := s.Paths["/Items/{itemId}"]["get"]
	if !ok {
		t.Fatal("GET /Items/{itemId} missing")
	}
	if len(items.Security) == 0 {
		t.Error("authenticated route has no security requirement")
	}
	if _, ok := items.Responses["403"]; ok {
		t.Error("non-admin route should not document 403")
	}
	if len(items.Parameters) != 1 || items.Parameters[0].Name != "itemId" || items.Parameters[0].In != "path" {
		t.Errorf("parameters = %+v, want the itemId path parameter", items.Parameters)
	}

	// The catch-all is not an endpoint.
	if _, ok := s.Paths["/"]; ok {
		t.Error("catch-all \"/\" should not be in the spec")
	}

	ids := map[string]bool{}
	for path, ops := range s.Paths {
		for method, op := range ops {
			if ids[op.OperationID] {
				t.Errorf("duplicate operationId %q (%s %s)", op.OperationID, method, path)
			}
			ids[op.OperationID] = true
		}
	}
}

func TestOpenAPIHonorsBaseURL(t *testing.T) {
	e := newTestEnv(t)
	e.srv.cfg.Server.BaseURL = "/jellyfin"
	e.handler = e.srv.routes()

	w := e.do(t, "GET", "/jellyfin/api-docs/openapi.json", "", nil)
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	var s spec
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Servers) != 1 || s.Servers[0].URL != "/jellyfin" {
		t.Errorf("servers = %+v, want /jellyfin", s.Servers)
	}
}

func TestSwaggerUIServed(t *testing.T) {
	e := newTestEnv(t)
	for _, p := range []string{"/api-docs/swagger", "/api-docs/swagger/"} {
		w := e.do(t, "GET", p, "", nil)
		if w.Code != 200 {
			t.Errorf("%s: status %d", p, w.Code)
			continue
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: content-type %q", p, ct)
		}
		if !strings.Contains(w.Body.String(), "SwaggerUIBundle") {
			t.Errorf("%s: body is not the Swagger UI page", p)
		}
	}
}
