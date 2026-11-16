package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testWebIndex  = "<html>web</html>"
	testWebScript = "console.log('web')"
	testWebSecret = "OUTSIDE-THE-WEB-FOLDER"
)

// newWebEnv is newTestEnv with a small stand-in for a jellyfin-web build on
// disk and the web client switched on. mutate runs before the routes are
// built, since that is when the web folder is opened.
func newWebEnv(t *testing.T, mutate func(*Config)) *testEnv {
	t.Helper()
	parent := t.TempDir()
	dir := filepath.Join(parent, "web")
	for path, body := range map[string]string{
		"index.html":    testWebIndex,
		"main.js":       testWebScript,
		"fonts/a.woff2": "woff2-bytes",
		"empty/.keep":   "",
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Something worth stealing, beside the web folder rather than in it, and
	// a symlink inside the folder that points at it.
	secret := filepath.Join(parent, "gofin.json")
	if err := os.WriteFile(secret, []byte(testWebSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "leak")); err != nil {
		t.Fatal(err)
	}

	e := newTestEnv(t)
	e.srv.cfg.Web.Enabled = true
	e.srv.cfg.Web.Dir = dir
	if mutate != nil {
		mutate(e.srv.cfg)
	}
	e.handler = e.srv.routes()
	return e
}

// Jellyfin Desktop opens a server by asking for "/" -- HEAD, then GET -- and
// expects to be sent to the web interface. A 501 here is the whole of "Error
// loading client".
func TestRootRedirectsToWeb(t *testing.T) {
	e := newWebEnv(t, nil)
	for _, method := range []string{"GET", "HEAD"} {
		w := e.do(t, method, "/", "", nil)
		if w.Code != 302 {
			t.Errorf("%s / = %d, want 302", method, w.Code)
		}
		if got := w.Header().Get("Location"); got != "/web/" {
			t.Errorf("%s / Location = %q, want /web/", method, got)
		}
	}
}

// stripBaseURL removes the prefix before routing, so a redirect built from the
// request path would send a proxied client to /web/, outside the proxy's
// prefix. Every redirect has to put it back.
func TestRootRedirectKeepsBaseURL(t *testing.T) {
	e := newWebEnv(t, func(c *Config) { c.Server.BaseURL = "/jellyfin" })
	for _, path := range []string{"/jellyfin", "/jellyfin/", "/jellyfin/web"} {
		w := e.do(t, "GET", path, "", nil)
		if w.Code != 302 {
			t.Errorf("GET %s = %d, want 302", path, w.Code)
		}
		if got := w.Header().Get("Location"); got != "/jellyfin/web/" {
			t.Errorf("GET %s Location = %q, want /jellyfin/web/", path, got)
		}
	}
	if w := e.do(t, "GET", "/jellyfin/web/", "", nil); w.Code != 200 || w.Body.String() != testWebIndex {
		t.Errorf("GET /jellyfin/web/ = %d %q, want the index", w.Code, w.Body.String())
	}
}

// With no web interface there is still something useful at "/": the API
// description, which is where upstream sends a headless server too.
func TestRootRedirectsToDocsWhenWebOff(t *testing.T) {
	e := newTestEnv(t)
	w := e.do(t, "GET", "/", "", nil)
	if w.Code != 302 {
		t.Errorf("GET / = %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != "/api-docs/swagger" {
		t.Errorf("Location = %q, want /api-docs/swagger", got)
	}
	for _, path := range []string{"/web", "/web/", "/web/index.html"} {
		if w := e.do(t, "GET", path, "", nil); w.Code != 404 {
			t.Errorf("GET %s with the web client off = %d, want 404", path, w.Code)
		}
	}
}

func TestWebServesIndex(t *testing.T) {
	e := newWebEnv(t, nil)
	for _, path := range []string{"/web/", "/web/index.html"} {
		w := e.do(t, "GET", path, "", nil)
		if w.Code != 200 {
			t.Fatalf("GET %s = %d, want 200", path, w.Code)
		}
		if got := w.Body.String(); got != testWebIndex {
			t.Errorf("GET %s body = %q, want the index", path, got)
		}
		if got := w.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s Content-Type = %q", path, got)
		}
		// The index names the hashed bundles, so a cached copy of it pins a
		// client to an interface build that may no longer be on disk.
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", path, got)
		}
	}
	if w := e.do(t, "GET", "/web", "", nil); w.Code != 302 || w.Header().Get("Location") != "/web/" {
		t.Errorf("GET /web = %d -> %q, want 302 -> /web/", w.Code, w.Header().Get("Location"))
	}
}

// The types come from gofin's own table: the container image has no MIME
// database, and a font or script served as text/plain is refused by a browser.
func TestWebContentTypes(t *testing.T) {
	e := newWebEnv(t, nil)
	for path, want := range map[string]string{
		"/web/main.js":       "text/javascript; charset=utf-8",
		"/web/fonts/a.woff2": "font/woff2",
	} {
		w := e.do(t, "GET", path, "", nil)
		if w.Code != 200 {
			t.Errorf("GET %s = %d, want 200", path, w.Code)
		}
		if got := w.Header().Get("Content-Type"); got != want {
			t.Errorf("GET %s Content-Type = %q, want %q", path, got, want)
		}
		if got := w.Header().Get("Cache-Control"); got != "" {
			t.Errorf("GET %s Cache-Control = %q, want none", path, got)
		}
	}
}

func TestWebContentTypeTable(t *testing.T) {
	for name, want := range map[string]string{
		"a.css":          "text/css; charset=utf-8",
		"a.JSON":         "application/json; charset=utf-8",
		"a.js.map":       "application/json; charset=utf-8",
		"a.svg":          "image/svg+xml",
		"a.ico":          "image/x-icon",
		"a.ttf":          "font/ttf",
		"a.eot":          "application/vnd.ms-fontobject",
		"a.wasm":         "application/wasm",
		"a.data":         "application/octet-stream",
		"a.mem":          "application/octet-stream",
		"a.webmanifest":  "application/manifest+json",
		"no-extension":   "",
		"a.unknown-type": "",
	} {
		if got := webContentType(name); got != want {
			t.Errorf("webContentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestWebHeadAndRange(t *testing.T) {
	e := newWebEnv(t, nil)

	w := e.do(t, "HEAD", "/web/main.js", "", nil)
	if w.Code != 200 {
		t.Errorf("HEAD = %d, want 200", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD carried a %d-byte body", w.Body.Len())
	}
	if got := w.Header().Get("Content-Length"); got != "18" {
		t.Errorf("HEAD Content-Length = %q, want 18", got)
	}

	r := httptest.NewRequest("GET", "/web/main.js", nil)
	r.Header.Set("Range", "bytes=0-1")
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, r)
	if rec.Code != 206 {
		t.Errorf("ranged GET = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "co" {
		t.Errorf("ranged GET body = %q, want %q", got, "co")
	}
}

// A miss is a plain 404. It must not reach the 501 catch-all (the interface
// probes for optional files, and each probe would log a warning), must not
// list a directory, and must never hand over a file from outside the folder.
func TestWebMisses(t *testing.T) {
	e := newWebEnv(t, nil)
	for _, path := range []string{
		"/web/nope.js",
		"/web/empty/",
		"/web/empty",
		"/web/leak",
	} {
		t.Run(path, func(t *testing.T) {
			w := e.do(t, "GET", path, "", nil)
			if w.Code != 404 {
				t.Errorf("status = %d, want 404", w.Code)
			}
			body := w.Body.String()
			if strings.Contains(body, testWebSecret) {
				t.Error("served a file from outside the web folder")
			}
			if strings.Contains(body, "Not implemented") {
				t.Error("fell through to the 501 catch-all")
			}
			if strings.Contains(body, ".keep") {
				t.Error("listed a directory")
			}
		})
	}
}

// Dot segments never reach the file system as written: the mux redirects them
// to the cleaned path, which is outside /web/. What matters is that no
// spelling of "one level up" returns the file.
func TestWebNoEscapeWithDotSegments(t *testing.T) {
	e := newWebEnv(t, nil)
	for _, path := range []string{
		"/web/../gofin.json",
		"/web/fonts/../../gofin.json",
		"/web//../gofin.json",
	} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			// Set directly so the segments arrive as sent rather than being
			// cleaned by the test's own URL parser.
			r.URL.Path = path
			w := httptest.NewRecorder()
			e.handler.ServeHTTP(w, r)
			if w.Code == 200 {
				t.Errorf("status = 200, want anything but")
			}
			if strings.Contains(w.Body.String(), testWebSecret) {
				t.Error("served a file from outside the web folder")
			}
		})
	}
}

// A wrong web.dir is a configuration mistake about the interface. It must not
// cost the API as well: the server starts, says so, and runs headless.
func TestWebDirWithoutIndexDisablesWeb(t *testing.T) {
	for name, dir := range map[string]string{
		"empty directory":   t.TempDir(),
		"missing directory": filepath.Join(t.TempDir(), "absent"),
	} {
		t.Run(name, func(t *testing.T) {
			e := newWebEnv(t, func(c *Config) { c.Web.Dir = dir })
			w := e.do(t, "GET", "/", "", nil)
			if w.Code != 302 || w.Header().Get("Location") != "/api-docs/swagger" {
				t.Errorf("GET / = %d -> %q, want 302 -> /api-docs/swagger", w.Code, w.Header().Get("Location"))
			}
			if w := e.do(t, "GET", "/web/", "", nil); w.Code != 404 {
				t.Errorf("GET /web/ = %d, want 404", w.Code)
			}
			if w := e.do(t, "GET", "/System/Info/Public", "", nil); w.Code != 200 {
				t.Errorf("the API went down with the web folder: %d", w.Code)
			}
		})
	}
}

func TestWebRejectsWrites(t *testing.T) {
	e := newWebEnv(t, nil)
	for _, path := range []string{"/", "/web/index.html"} {
		if w := e.do(t, "POST", path, "", nil); w.Code != 405 {
			t.Errorf("POST %s = %d, want 405", path, w.Code)
		}
	}
}

// The web folder is files, not API: it has no place in the generated spec.
func TestWebNotInOpenAPI(t *testing.T) {
	e := newWebEnv(t, nil)
	w := e.do(t, "GET", "/api-docs/openapi.json", "", nil)
	if w.Code != 200 {
		t.Fatalf("openapi.json = %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `"/web`) {
		t.Error("the OpenAPI document describes the web folder")
	}
}

// The container image turns the interface on with nothing but this variable,
// so that a deployed service needs no edit to its config file. Naming the
// directory is the request to serve it, as it already is for -webdir.
func TestWebDirEnvEnablesWeb(t *testing.T) {
	t.Setenv("JELLYFIN_WEB_DIR", "/x")
	fs := newFlagSet()
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Dir != "/x" || !cfg.Web.Enabled {
		t.Errorf("web = %+v, want dir /x and enabled", cfg.Web)
	}
}

// Flags are the last layer, so -no-web still switches the interface off in an
// image that sets the variable.
func TestNoWebFlagWins(t *testing.T) {
	t.Setenv("JELLYFIN_WEB_DIR", "/x")
	fs := newFlagSet()
	if err := fs.Parse([]string{"-no-web"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := load(fs, "")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Web.Enabled {
		t.Error("-no-web did not switch the web client off")
	}
}
