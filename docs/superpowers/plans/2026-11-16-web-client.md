# Web Client Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** gofin serves the official Jellyfin web interface under `/web/` and answers every API call that interface makes on the everyday path, so Jellyfin Desktop works against it.

**Architecture:** A new `cmd/gofin/web.go` registers three method-less patterns on the existing `apiMux` (`/{$}`, `/web`, `/web/`) and serves files from `cfg.Web.Dir` through an `os.Root`. The container image gains a build stage that fetches the pinned upstream release and copies its `jellyfin-web` folder in. The API gap is closed by running the interface in a browser against a local build and implementing each route it reports, in new files beside `main.go`.

**Tech Stack:** Go 1.25.0 (the `go.mod` floor; nothing newer than 1.25 in language or stdlib), standard library only, `modernc.org/sqlite` already present. Podman for the image. Playwright (MCP) for driving the browser.

**Spec:** `docs/superpowers/specs/2026-11-16-web-client-design.md`

## Global Constraints

- `CGO_ENABLED=0` static binary. No new Go dependency.
- Go 1.25.0 is the floor in `go.mod`; the local toolchain is newer, so do not use anything added after 1.25.
- Every existing test keeps passing. No existing route changes its response without a test saying why.
- Unknown data is never invented: real data where gofin has it, otherwise the empty-but-valid shape upstream returns, with a comment naming what is missing.
- New handlers go in new files beside `main.go`, not into it.
- Web interface build: Jellyfin `10.11.11`, from `https://repo.jellyfin.org/files/server/portable/stable/v10.11.11/any/jellyfin_10.11.11.tar.xz`, checksum-pinned.
- Web interface files are never committed to the repository.
- Upstream reference for route shapes: `/var/home/deanvlue/dev/jellyfin` (`Jellyfin.Api/Controllers/`, `MediaBrowser.Model/`).
- **No commits.** Carlos has not asked for any, and the tree holds unrelated uncommitted work. Leave everything in the working tree.

## Review Focus

Each line is pinned by a test in Task 1.

1. **`base_url` is set** (`/jellyfin`): `/jellyfin`, `/jellyfin/` and `/jellyfin/web` must redirect to `/jellyfin/web/`, not to `/web/`.
2. **Escape attempts** (`/web/../gofin.json`, a symlink inside the web folder pointing outside it): 404, never file contents.
3. **`HEAD` and `Range` on a static file**: `HEAD` returns headers and no body; a range request returns `206` with the slice.
4. **Web folder missing or without `index.html` at startup**: the API still serves, `/` redirects to `api-docs/swagger`, `/web/` is 404.
5. **A miss under `/web/`**, including a directory with no `index.html`: plain 404, no listing, no "unimplemented endpoint" warning.

## File Structure

| File | Responsibility |
|---|---|
| `cmd/gofin/web.go` (new) | Root redirect, `/web` tree, content-type table |
| `cmd/gofin/web_test.go` (new) | Tests for the above |
| `cmd/gofin/main.go` | One call to `s.registerWeb(mux)`; `JELLYFIN_WEB_DIR` enables the web client |
| `cmd/gofin/webapi*.go` (new, as needed) | Routes the web interface needs, grouped by upstream controller |
| `cmd/gofin/webapi*_test.go` (new) | Handler tests for those routes |
| `Dockerfile` | `web` build stage, copy, `ENV JELLYFIN_WEB_DIR` |
| `gofin.example.json`, `README.md` | Document the `web` key, the image, and the new state of things |

---

### Task 1: Static serving and the root redirect

**Files:**
- Create: `cmd/gofin/web.go`, `cmd/gofin/web_test.go`
- Modify: `cmd/gofin/main.go` (in `routes()`, before the `/` catch-all)

**Interfaces:**
- Produces: `func (s *Server) registerWeb(mux *apiMux)`; `func webContentType(name string) string` (empty string means "let `http.ServeContent` sniff").
- Consumes: `s.cfg.Web.Enabled`, `s.cfg.Web.Dir`, `s.cfg.Server.BaseURL`, `s.log`.

Behaviour to implement:

- `registerWeb` opens `os.OpenRoot(cfg.Web.Dir)` when `Web.Enabled`. If that fails or the root has no regular file `index.html`, log at error with the directory and treat the web client as off.
- `/{$}`: `GET`/`HEAD` answer `302` with `Location: <BaseURL>/web/` when on, `<BaseURL>/api-docs/swagger` when off. Other methods: `405`. The `Location` is built from `cfg.Server.BaseURL` by hand, not with `http.Redirect`, because `stripBaseURL` has already removed the prefix from `r.URL.Path`.
- `/web`: `302` to `<BaseURL>/web/` when on.
- `/web/`: `GET`/`HEAD` only. Clean the path, open it inside the root; a directory resolves to its `index.html`; anything missing is `http.NotFound` logged at debug. Serve with `http.ServeContent` (gives `Last-Modified`, conditional requests, `HEAD`, ranges). Set `Content-Type` from `webContentType` first. `index.html` gets `Cache-Control: no-cache`.
- When off, `/web` and `/web/` answer plain 404.
- `webContentType` table: `.html .js .mjs .css .json .map .svg .png .jpg .jpeg .gif .webp .ico .woff .woff2 .ttf .otf .wasm .txt .xml .webmanifest .mp3 .ogg`, plus `.data` and `.mem` as `application/octet-stream`. Text types carry `; charset=utf-8`.

- [ ] **Step 1: Write the failing tests** in `web_test.go`. A helper `newWebEnv(t, mutate func(*Config)) *testEnv` builds a temp web folder containing `index.html` (`<html>web</html>`), `main.js`, `fonts/a.woff2`, `empty/` (a directory with no index), and a symlink `leak` pointing at a file outside the folder; it sets `cfg.Web.Enabled=true`, `cfg.Web.Dir`, applies `mutate`, then builds the server the way `newTestEnv` does. Tests:

| Test | Request | Asserts |
|---|---|---|
| `TestRootRedirectsToWeb` | `GET /`, `HEAD /` | 302, `Location: /web/` |
| `TestRootRedirectKeepsBaseURL` | `BaseURL=/jellyfin`; `GET /jellyfin`, `/jellyfin/`, `/jellyfin/web` | 302, `Location: /jellyfin/web/` |
| `TestRootRedirectsToDocsWhenWebOff` | web off; `GET /` | 302, `Location: /api-docs/swagger`; `GET /web/` is 404 |
| `TestWebServesIndex` | `GET /web/`, `GET /web/index.html` | 200, body `<html>web</html>`, `Content-Type: text/html; charset=utf-8`, `Cache-Control: no-cache` |
| `TestWebContentTypes` | `GET /web/main.js`, `/web/fonts/a.woff2` | `text/javascript; charset=utf-8`, `font/woff2`; no `Cache-Control: no-cache` |
| `TestWebHeadAndRange` | `HEAD /web/main.js`; `GET` with `Range: bytes=0-1` | HEAD: 200, empty body, `Content-Length` set. Range: 206, two bytes |
| `TestWebMisses` | `GET /web/nope.js`, `/web/empty/`, `/web/../gofin.json`, `/web/leak` | each 404, body is not a problem document with status 501, and never the outside file's contents |
| `TestWebDirWithoutIndexDisablesWeb` | `Web.Dir` = empty temp dir | `GET /` goes to `/api-docs/swagger`; `GET /System/Info/Public` still 200 |
| `TestWebRejectsWrites` | `POST /web/index.html`, `POST /` | 405 |
| `TestWebNotInOpenAPI` | `GET /api-docs/openapi.json` | no path beginning `/web` |

- [ ] **Step 2: Run them and see them fail.** `go test ./cmd/gofin -run 'TestRoot|TestWeb' -count=1`. Expected: compile failure, `registerWeb` undefined (after the helper is written, 501s).
- [ ] **Step 3: Implement `web.go`** and add `s.registerWeb(mux)` to `routes()`.
- [ ] **Step 4: Run them and see them pass**, then the whole suite: `go vet ./... && go test -race -count=1 ./...`. Expected: all pass. If the mux panics on a pattern conflict at registration, that is the finding to fix, not to work around by reordering.

### Task 2: Configuration

**Files:**
- Modify: `cmd/gofin/main.go` (the `JELLYFIN_*` env block), `cmd/gofin/main_test.go` or `web_test.go`, `gofin.example.json`

**Interfaces:**
- Consumes: the existing config loader and its test style (find the loader's name in `main.go` around the env block; reuse whatever the existing config tests call).

- [ ] **Step 1: Write the failing test** `TestWebDirEnvEnablesWeb`: with `JELLYFIN_WEB_DIR=/x` set via `t.Setenv` and no config file, the loaded config has `Web.Dir == "/x"` and `Web.Enabled == true`. And `TestNoWebFlagWins`: the same env plus `-no-web` gives `Web.Enabled == false`.
- [ ] **Step 2: Run, see the first fail.**
- [ ] **Step 3: Implement:** a non-empty `JELLYFIN_WEB_DIR` sets `Web.Enabled = true`.
- [ ] **Step 4: Run, see both pass.**
- [ ] **Step 5: Update `gofin.example.json`'s `web` commentary** to say what `dir` must contain (an unpacked `jellyfin-web` folder with `index.html`), that `JELLYFIN_WEB_DIR` / `--webdir` also enable it, and that the container image already sets it.

### Task 3: Container image

**Files:**
- Modify: `Dockerfile`

- [ ] **Step 1: Download the archive once into the scratchpad**, compute its SHA-256, and list its top-level layout to learn the path of the `jellyfin-web` folder inside it. Keep the extracted `jellyfin-web` folder in the scratchpad: Task 4 serves it.
- [ ] **Step 2: Add a `web` stage** (`alpine`, `curl`, `xz`, `tar`) with `ARG JELLYFIN_VERSION=10.11.11` and `ARG JELLYFIN_SHA256=<from step 1>`: download, `sha256sum -c`, extract only the `jellyfin-web` folder to `/jellyfin-web`.
- [ ] **Step 3: In the final stage**, `COPY --from=web /jellyfin-web /usr/share/jellyfin-web` and `ENV JELLYFIN_WEB_DIR=/usr/share/jellyfin-web`.
- [ ] **Step 4: Verify:** `podman build -t localhost/gofin:web-test .` succeeds; `podman run --rm localhost/gofin:web-test ls /usr/share/jellyfin-web/index.html` prints the path. Use the `web-test` tag so the running service's image is untouched until Task 5.

### Task 4: Close the API gap

**Files:**
- Create: `cmd/gofin/webapi*.go` and matching `_test.go` files, grouped by upstream controller
- Modify: `cmd/gofin/main.go` only to register routes

**Setup (once):**
- Build the tree to the scratchpad. Write a scratch config: port `8097`, `data`/`cache`/`logs` under the scratchpad, the two real libraries (`/mnt/alberca/Movies`, `/mnt/alberca/Music`, read-only), TMDB **off** (no network traffic on Carlos's key), one admin user with a throwaway password, `web.dir` pointing at the folder from Task 3 step 1, log level `debug`. Copy the live poster cache (`~/.local/share/gofin/cache/images`) into the scratch cache so artwork shows.

**The loop:**

- [ ] **Step 1: Run the path in a browser** at `http://localhost:8097/`: load, log in, home screen, open Movies, open Music, open a film's detail page, open an album, play a track, play a film whose container a browser can decode (pick an `.mp4` if the library has one), log out. After each screen, collect: "unimplemented endpoint" warnings from the server log, failed network requests, and console errors.
- [ ] **Step 2: For each finding**, read the upstream controller action and its response type; write a handler test asserting status and the fields the interface reads; see it fail; implement; see it pass. Use `authenticate(..., false)` unless upstream requires elevation.
- [ ] **Step 3: Rebuild, restart the local server, and repeat step 1** until one full pass produces no unimplemented-endpoint warning, no unexpected 4xx/5xx, and no console error traceable to a server response.
- [ ] **Step 4: Full suite:** `go vet ./... && go test -race -count=1 ./...`.

**Rules for step 2** (from the spec): real data where gofin has it; empty-but-valid shape otherwise; writes gofin cannot honour are read and answered `204`. If the interface misbehaves because display preferences do not persist, wire `GET`/`POST /DisplayPreferences/{prefsId}` to the existing `jfdb` prefs repository, keyed on user, prefs id and client. If the interface refuses server version `10.11.0`, raise the reported version to `10.11.11` and say so in the README.

**Done when:** step 3's clean pass is observed and recorded (the list of routes added goes in the final report).

### Task 5: Documentation and rollout

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README.** Add a "Web interface" section (what is served, where the files come from, how to point `web.dir` at a folder outside the container, that films a browser cannot decode need Jellyfin Desktop because there is no transcoding). Update the "What is actually implemented" table with the new rows, the test counts, and the sentence saying `GET /` is unimplemented if one exists.
- [ ] **Step 2: Build the real image:** `podman build -t localhost/gofin:latest .`
- [ ] **Step 3: Restart the service:** `systemctl --user restart gofin`, then `systemctl --user is-active gofin`. Expected: `active`.
- [ ] **Step 4: Verify the deployed service:** `curl -sI localhost:8096/` shows `302` and `Location: /web/`; `curl -s -o /dev/null -w '%{http_code}' localhost:8096/web/index.html` prints `200`; the journal shows the libraries scanned and no error about the web folder.
- [ ] **Step 5: Run the Task 4 browser path once more against port 8096** with the journal open. Expected: no unimplemented-endpoint warning.
- [ ] **Step 6: Hand to Carlos** for the Jellyfin Desktop check, including film playback.
