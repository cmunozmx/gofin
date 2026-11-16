package main

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
)

// The web interface. The official clients that are not native apps -- Jellyfin
// Desktop, the Android and iOS wrappers, a browser -- do not ship an interface
// of their own: they ask the server for "/" and load whatever it serves under
// /web/. Upstream serves a jellyfin-web build from disk there
// ($JF/Jellyfin.Server/Startup.cs, HostWebClient), and so does this.
//
// The files are not part of this repository or this binary. They are a
// directory named by web.dir; the container image fills it from the upstream
// release at build time.

// webContentTypes is the whole of what a jellyfin-web build contains, spelled
// out rather than left to mime.TypeByExtension: that consults the host's MIME
// database, the Alpine image has none, and Go's built-in fallback knows
// neither fonts nor icons. A browser refuses a script or a font served as
// text/plain, so a missing entry here is a blank page rather than a cosmetic
// fault.
var webContentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".wasm":        "application/wasm",
	".mp3":         "audio/mpeg",
	".ogg":         "audio/ogg",
	// The subtitle renderer's compiled assets. Upstream adds exactly these two.
	".data": "application/octet-stream",
	".mem":  "application/octet-stream",
}

// webContentType returns the Content-Type for a file in the web folder, or ""
// for an extension the table does not know, which leaves it to be sniffed.
func webContentType(name string) string {
	return webContentTypes[strings.ToLower(path.Ext(name))]
}

// openWebRoot opens the web folder, or returns nil when there is no usable
// one. A bad web.dir is reported and survived: it is a mistake about the
// interface, and refusing to start would take the API down for every client
// that never wanted the interface in the first place.
func (s *Server) openWebRoot() *os.Root {
	if !s.cfg.Web.Enabled {
		return nil
	}
	dir := s.cfg.Web.Dir
	if dir == "" {
		s.log.Error("web client is enabled but web.dir is not set; running API-only")
		return nil
	}
	// os.Root, rather than joining paths under dir: it refuses any name that
	// resolves outside the directory, including through a symlink inside it.
	root, err := os.OpenRoot(dir)
	if err != nil {
		s.log.Error("cannot open the web client directory; running API-only", "dir", dir, "err", err)
		return nil
	}
	if st, err := root.Stat("index.html"); err != nil || !st.Mode().IsRegular() {
		s.log.Error("web client directory has no index.html; running API-only", "dir", dir)
		_ = root.Close()
		return nil
	}
	return root
}

// registerWeb adds "/" and the /web/ tree. The patterns carry no method, so
// apiMux does not record them: they are files, not API endpoints.
func (s *Server) registerWeb(mux *apiMux) {
	root := s.openWebRoot()

	// stripBaseURL has already removed the reverse-proxy prefix from the
	// request path, so http.Redirect -- which resolves against that path --
	// would send a proxied client outside its prefix. The targets are built
	// from the configured prefix instead.
	base := s.cfg.Server.BaseURL
	webHome := base + "/web/"

	// With no interface there is still something worth landing on, and it is
	// where upstream sends a server that does not host the web client.
	home := base + "/api-docs/swagger"
	if root != nil {
		home = webHome
	}
	mux.HandleFunc("/{$}", readOnly(redirectTo(home)))

	if root == nil {
		// Registered rather than left to the catch-all: "there is no web
		// interface here" is a 404, not an unimplemented API endpoint.
		mux.HandleFunc("/web", http.NotFound)
		mux.HandleFunc("/web/", http.NotFound)
		return
	}
	mux.HandleFunc("/web", readOnly(redirectTo(webHome)))
	mux.HandleFunc("/web/", readOnly(serveWebFile(root)))
}

// readOnly admits GET and HEAD and answers anything else 405.
func readOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
}

func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusFound)
	}
}

func serveWebFile(root *os.Root) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/web/")
		if name == "" {
			name = "index.html"
		}
		// The mux has already redirected any path with dot segments, and
		// os.Root would refuse an escape anyway; this is the cheap check
		// that makes both of those a second line of defence, not the first.
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		f, err := root.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() {
			// A directory is a miss: nothing in a jellyfin-web build is
			// addressed as one, and listing it would be a disclosure.
			http.NotFound(w, r)
			return
		}

		h := w.Header()
		if ct := webContentType(name); ct != "" {
			h.Set("Content-Type", ct)
		}
		// The index names the content-hashed bundles. A cached copy pins a
		// client to a build that an upgrade has since removed from disk, so
		// it is revalidated every time, as upstream does. Everything else
		// goes out with Last-Modified and is revalidated on the client's
		// own schedule.
		if path.Base(name) == "index.html" {
			h.Set("Cache-Control", "no-cache")
		}
		http.ServeContent(w, r, name, st.ModTime(), f)
	}
}
