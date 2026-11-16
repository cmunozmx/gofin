package main

import (
	"net/http"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Case-insensitive routing.
//
// ASP.NET Core matches route templates without regard to case, so on a real
// server /users/public and /Users/Public are the same endpoint -- and clients
// have come to rely on it. The web interface asks for /users/public and posts
// to /Users/authenticatebyname. Go's ServeMux compares exactly, which made
// each of those an unimplemented endpoint.
//
// Rather than registering every spelling a client has been seen to use, the
// request path is normalised to the registered spelling before it reaches the
// mux. Only the fixed segments of a route are touched: whatever sits in a
// wildcard position is the client's data (an id, a file name, a preferences
// key) and is passed through as sent.

type caseRoute struct {
	method   string
	segs     []string // "" marks a wildcard position
	rest     bool     // the last segment is a {name...} remainder
	literals int
}

type caseRouter struct{ routes []caseRoute }

// newCaseRouter is built from the routes apiMux recorded, so it knows exactly
// the method-qualified API endpoints. Method-less patterns -- the catch-all
// and the web folder -- are not recorded and are therefore never rewritten;
// file names under /web/ stay case-sensitive, as they are upstream on Linux.
func newCaseRouter(routes []apiRoute) *caseRouter {
	c := &caseRouter{}
	for _, rt := range routes {
		cr := caseRoute{method: rt.method}
		for _, seg := range strings.Split(strings.TrimPrefix(rt.path, "/"), "/") {
			switch {
			case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}"):
				cr.rest = true
			case strings.HasPrefix(seg, "{"):
				cr.segs = append(cr.segs, "")
			default:
				cr.segs = append(cr.segs, seg)
				cr.literals++
			}
		}
		c.routes = append(c.routes, cr)
	}
	return c
}

func (cr *caseRoute) matches(req []string) bool {
	if len(req) < len(cr.segs) || (!cr.rest && len(req) != len(cr.segs)) {
		return false
	}
	for i, seg := range cr.segs {
		if seg != "" && !strings.EqualFold(seg, req[i]) {
			return false
		}
	}
	return true
}

// canonical returns p with the fixed segments of the best-matching route
// spelled the way that route was registered, or p unchanged when no route
// matches.
//
// "Best" mirrors the mux's own precedence: more literal segments win, so
// /items/latest becomes /Items/Latest rather than matching /Items/{itemId}. A
// route for the request's own method is preferred, but one for another method
// still normalises the path, so that the mux answers 405 for it rather than
// the catch-all answering 501.
func (c *caseRouter) canonical(method, p string) string {
	if method == http.MethodHead {
		method = http.MethodGet
	}
	req := strings.Split(strings.TrimPrefix(p, "/"), "/")
	var best *caseRoute
	bestScore := -1
	for i := range c.routes {
		cr := &c.routes[i]
		if !cr.matches(req) {
			continue
		}
		score := cr.literals * 2
		if cr.method == method {
			score += 1 << 16
		}
		if !cr.rest {
			score++ // an exact-length route is more specific than a remainder
		}
		if score > bestScore {
			best, bestScore = cr, score
		}
	}
	if best == nil {
		return p
	}
	changed := false
	for i, seg := range best.segs {
		if seg != "" && req[i] != seg {
			req[i] = seg
			changed = true
		}
	}
	if !changed {
		return p
	}
	return "/" + strings.Join(req, "/")
}

// aliasQueryCase makes query parameter names readable in either of the two
// spellings clients use. ASP.NET binds them without regard to case, and the
// web interface alone sends both: its older code asks for ParentId, the newer
// SDK-generated code for parentId. Handlers read one spelling with
// url.Values.Get, which is exact.
//
// So each name is also made available with the case of its first letter
// flipped. That is not full case-insensitivity -- PARENTID is still unknown --
// but PascalCase and camelCase are the two spellings that exist in practice.
// A name the client sent in both forms is left alone, and a query with nothing
// to add is returned byte for byte.
func aliasQueryCase(raw string) string {
	if raw == "" {
		return raw
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	added := false
	for k, v := range q {
		if k == "" {
			continue
		}
		first, size := utf8.DecodeRuneInString(k)
		flipped := unicode.ToLower(first)
		if flipped == first {
			flipped = unicode.ToUpper(first)
		}
		if flipped == first {
			continue // a digit or an underscore: there is no other spelling
		}
		alt := string(flipped) + k[size:]
		if _, ok := q[alt]; !ok {
			q[alt] = v
			added = true
		}
	}
	if !added {
		return raw
	}
	return q.Encode()
}

func (c *caseRouter) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.RawQuery = aliasQueryCase(r.URL.RawQuery)
		if p := c.canonical(r.Method, r.URL.Path); p != r.URL.Path {
			r.URL.Path = p
			// The escaped form, if any, describes the old spelling; cleared
			// so the mux derives it from the path it should now match.
			r.URL.RawPath = ""
		}
		next.ServeHTTP(w, r)
	})
}
