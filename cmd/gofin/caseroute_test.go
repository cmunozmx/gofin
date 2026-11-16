package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ASP.NET Core matches routes without regard to case, and the web interface
// leans on it: it asks for /users/public and posts to
// /Users/authenticatebyname. Go's mux is exact, so each of those was a 501.
func TestRoutesMatchWithoutRegardToCase(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/users/public", "", 200},
		{"GET", "/SYSTEM/INFO/PUBLIC", "", 200},
		{"GET", "/system/ping", "", 200},
		{"HEAD", "/system/info/public", "", 200},
		{"GET", "/users/me", tok, 200},
		{"GET", "/userviews", tok, 200},
		{"GET", "/Users/" + e.viewer.ID.String() + "/views", tok, 200},
	} {
		if w := e.do(t, c.method, c.path, c.token, nil); w.Code != c.want {
			t.Errorf("%s %s = %d, want %d", c.method, c.path, w.Code, c.want)
		}
	}
}

// A literal segment beats a wildcard in the same position, in any case:
// /items/latest is the Latest list, not a lookup of an item called "latest".
func TestCaseInsensitiveRoutingPrefersLiteralSegments(t *testing.T) {
	e := newTestEnv(t)
	tok := e.tokenFor(e.viewer)
	w := e.do(t, "GET", "/items/latest", tok, nil)
	if w.Code != 200 || !strings.HasPrefix(strings.TrimSpace(w.Body.String()), "[") {
		t.Errorf("GET /items/latest = %d %.60q, want 200 with a list", w.Code, w.Body.String())
	}
}

// Only the fixed parts of a route are case-normalised. Whatever sits in a
// wildcard position is the client's data and reaches the handler as sent.
func TestCanonicalPathLeavesWildcardValuesAlone(t *testing.T) {
	routes := []apiRoute{
		{method: "GET", path: "/DisplayPreferences/{prefsId}"},
		{method: "GET", path: "/Items/{itemId}"},
		{method: "GET", path: "/Items/Latest"},
		{method: "POST", path: "/Sessions/Playing"},
		{method: "GET", path: "/Videos/{itemId}/{mediaSourceId}/Subtitles/{index}/{file}"},
		{method: "GET", path: "/Audio/{itemId}/{rest...}"},
	}
	c := newCaseRouter(routes)
	for _, tc := range []struct{ method, in, want string }{
		{"GET", "/displaypreferences/UserSettings", "/DisplayPreferences/UserSettings"},
		{"GET", "/items/LATEST", "/Items/Latest"},
		{"GET", "/items/AbCd", "/Items/AbCd"},
		{"HEAD", "/items/latest", "/Items/Latest"},
		{"GET", "/videos/A/B/subtitles/3/Stream.VTT", "/Videos/A/B/Subtitles/3/Stream.VTT"},
		{"GET", "/audio/A/Main/Stream.MP3", "/Audio/A/Main/Stream.MP3"},
		// The right path under the wrong method is still normalised, so the
		// mux can answer 405 rather than the catch-all answering 501.
		{"GET", "/sessions/playing", "/Sessions/Playing"},
		// Nothing registered looks like this: left exactly as sent.
		{"GET", "/Nonsense/Path", "/Nonsense/Path"},
		{"GET", "/web/Main.JS", "/web/Main.JS"},
		{"GET", "/", "/"},
	} {
		if got := c.canonical(tc.method, tc.in); got != tc.want {
			t.Errorf("canonical(%s %s) = %q, want %q", tc.method, tc.in, got, tc.want)
		}
	}
}

// The rewrite must not disturb a request that was already spelled correctly,
// nor the query string of one that was not.
func TestCaseRouterKeepsQuery(t *testing.T) {
	c := newCaseRouter([]apiRoute{{method: "GET", path: "/Items/Latest"}})
	var got string
	h := c.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = r.URL.Path + " Limit=" + q.Get("Limit") + " ParentId=" + q.Get("ParentId")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/items/latest?Limit=5&ParentId=Ab", nil))
	if got != "/Items/Latest Limit=5 ParentId=Ab" {
		t.Errorf("handler saw %q", got)
	}
}

// Query parameter names are case-insensitive upstream too, and clients split
// between two spellings: the interface's older code sends ParentId, its newer
// code sends parentId. A handler reads one; it must see either.
func TestQueryNamesMatchInEitherCase(t *testing.T) {
	c := newCaseRouter(nil)
	var got map[string]string
	h := c.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		got = map[string]string{
			"ParentId": q.Get("ParentId"), "parentId": q.Get("parentId"),
			"Limit": q.Get("Limit"), "limit": q.Get("limit"),
			"api_key": q.Get("api_key"), "Api_key": q.Get("Api_key"),
		}
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x?parentId=abc&Limit=5&api_key=k%20v", nil))
	for k, want := range map[string]string{
		"ParentId": "abc", "parentId": "abc", "Limit": "5", "limit": "5", "api_key": "k v", "Api_key": "k v",
	} {
		if got[k] != want {
			t.Errorf("q.Get(%q) = %q, want %q", k, got[k], want)
		}
	}
}

// A client that sends both spellings means both; neither overwrites the other.
// And a request with nothing to add is passed through byte for byte.
func TestQueryAliasesDoNotOverwrite(t *testing.T) {
	if got := aliasQueryCase("limit=1&Limit=2"); got != "limit=1&Limit=2" {
		t.Errorf("both spellings present: rewritten to %q", got)
	}
	for _, raw := range []string{"", "%zz=broken", "=novalue", "1=2"} {
		if got := aliasQueryCase(raw); got != raw {
			t.Errorf("aliasQueryCase(%q) = %q, want it untouched", raw, got)
		}
	}
}
