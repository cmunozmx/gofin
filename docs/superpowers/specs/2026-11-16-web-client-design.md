# Serving the Jellyfin web client from gofin

Date: 2026-11-16. Status: awaiting approval.

## Intent

Jellyfin Desktop (flatpak, 2.0.0) is a shell around the web interface, which it
loads from the server it connects to. Pointed at gofin it issues `HEAD /` then
`GET /`, gets the 501 problem document, and shows "Error loading client". The
goal is for the official client to work against gofin the way it does against a
real server: the interface loads, and the everyday path through it works.

## Goals

1. `GET /` and `HEAD /` redirect to the web interface, and gofin serves that
   interface from a directory on disk under `/web/`.
2. The container image ships the official jellyfin-web build, so the deployed
   service works with no extra setup.
3. In the web interface, against the real Movies and Music libraries, this path
   works end to end: log in, home screen, open each library, open an item,
   play a film and a music track, log out.
4. No request on that path lands in the "unimplemented endpoint" catch-all.

## Non-goals

- Transcoding. A file the browser cannot decode will not play in a plain
  browser. Jellyfin Desktop plays through mpv, so it is the reference for
  video playback; a browser is the reference for everything else.
- The admin dashboard pages, user management, plugins, live TV, TV shows,
  SyncPlay, Quick Connect.
- Committing web interface files to this repository, or embedding them in the
  binary.
- Changing the interface itself.

## Anti-goals

- Breaking Delfin or Gelly. Every existing test keeps passing, and no existing
  route changes its response without a test that says why.
- A new Go dependency. Static file serving is standard library.
- Answering an unknown route with invented data. Where gofin has the data it
  returns it; where it does not, it returns the empty-but-valid shape upstream
  would (the convention already used for the TV rows).

## Constraints

- `CGO_ENABLED=0` static binary, standard library plus `modernc.org/sqlite`.
- gofin reports version `10.11.0`; the interface must be a 10.11 build.
- The upstream server source is at `/var/home/deanvlue/dev/jellyfin` (12.0-rc5)
  and is the reference for route shapes. The web interface source is not there.
- The working tree already has uncommitted work (SIGHUP rescan, OpenAPI docs).
  This change builds on top of it and leaves it intact.

## Approaches

| | Approach | Trade-off |
|---|---|---|
| A | Serve from a folder on disk; the container build fetches the official release and copies its web folder in | Binary stays small; interface upgrades do not touch Go. Needs a download at image build time. |
| B | Embed the interface in the binary | One file to ship, but tens of megabytes of third-party build in the binary and a recompile per interface upgrade. |
| C | Build jellyfin-web from source | Only useful for modifying the interface; adds a Node toolchain. |

**Chosen: A** (picked by Carlos). It is what upstream does
(`Jellyfin.Server/Startup.cs:200`), and the `web.enabled` / `web.dir` config
keys and the `--webdir` flag for it already exist, unused.

## Design

### Part 1: static serving

New file `cmd/gofin/web.go`, tests in `cmd/gofin/web_test.go`.

- **Root redirect.** `GET /` and `HEAD /` answer `302` with a *relative*
  `Location: web/` when the web client is enabled, as upstream's
  `BaseUrlRedirectionMiddleware` does. Relative, so a configured `base_url`
  prefix survives. When the web client is disabled, redirect to
  `api-docs/swagger` instead, mirroring upstream's fallback to its Swagger page.
- **`/web/` tree.** Served from `web.dir`. `/web` redirects to `/web/`;
  `/web/` serves `index.html`. No directory listings. Paths are confined to the
  directory (no `..` escape, no following a symlink out of it).
- **Headers.** `index.html` goes out with `Cache-Control: no-cache`, as
  upstream does. Everything else relies on `Last-Modified` and conditional
  requests.
- **Content types from an explicit table**, not the host's MIME database: the
  Alpine image has none, and Go's built-in table lacks `.woff2`, `.ttf`,
  `.ico`, `.map`. Also `.data` and `.mem` as `application/octet-stream`, which
  upstream adds for the subtitle renderer.
- **Misses.** A missing file under `/web/` is a plain `404`, logged at debug.
  It does not reach the 501 catch-all and does not log a warning.
- **No authentication**, as upstream: the login page is in there.
- **Not in the OpenAPI document.** These are files, not API endpoints.

### Part 2: configuration

- Setting `JELLYFIN_WEB_DIR` enables the web client as well as setting its
  directory, the way `--webdir` already does. `--no-web` still turns it off.
- At startup, if the web client is enabled and the directory has no
  `index.html`, log an error that names the directory and carry on API-only.
  A typo in a path should not take the API down with it.
- `gofin.example.json` and the README section on the `web` key are updated.

### Part 3: container image

- A new build stage downloads
  `https://repo.jellyfin.org/files/server/portable/stable/v10.11.11/any/jellyfin_10.11.11.tar.xz`,
  verifies it against a checksum pinned in the `Dockerfile`, and extracts only
  the `jellyfin-web` folder.
- The final stage copies that folder to `/usr/share/jellyfin-web` and sets
  `ENV JELLYFIN_WEB_DIR=/usr/share/jellyfin-web`.
- The version is a single `ARG` so an upgrade is a one-line change.
- `gofin.container.example` needs no change.

### Part 4: closing the API gap

The set of routes the interface calls is not known in advance and is found by
running it. The loop:

1. Build the current tree and run it on a spare port with a scratch data
   directory, the real libraries (read-only), and the web folder extracted from
   the same release archive.
2. Drive the goal-3 path in a real browser. Collect every "unimplemented
   endpoint" warning, every 4xx/5xx the interface did not expect, and every
   console error.
3. For each, read the upstream controller and its response type under
   `Jellyfin.Api/Controllers/` and `MediaBrowser.Model/`, implement the route
   in gofin, and add a handler test in the existing style.
4. Repeat until the path is clean.

Rules for step 3:

- Real data where gofin has it (items, user data, sessions' own token).
- Empty-but-valid shape where it does not, with a comment saying what is
  missing.
- Writes the interface makes that gofin cannot honour yet are accepted and
  answered `204`, as the display-preferences write is today. The exception is
  display preferences themselves: the interface stores its home-screen layout
  there, and the table and repository methods already exist, so wire them if
  the interface misbehaves without persistence.
- New handlers go in new files beside `main.go`, not into it.

### Part 5: verification and rollout

- `go vet ./...`, `go test ./...`, `go test -race ./...` all pass.
- The goal-3 path is run in a browser against the local build, with the server
  log showing no unimplemented-endpoint warnings.
- Rebuild `localhost/gofin:latest`, restart the `gofin` user service, and
  confirm `GET /` redirects and `/web/index.html` loads from the container.
  This redeploy also ships the uncommitted work already in the tree.
- Carlos opens Jellyfin Desktop against the redeployed service for the final
  check, including film playback.

## Left to the builder

- How handlers are grouped into files.
- Whether the interface needs gofin to report a newer `10.11.x` patch version;
  change it only if the interface refuses `10.11.0`.
- The exact list of routes, which Part 4 discovers.

## Calls made by Claude, not by Carlos

- `/` redirects to the API docs when the web client is off.
- `JELLYFIN_WEB_DIR` alone enables the web client.
- A bad web directory logs an error and the server starts API-only.
- Pinning 10.11.11 with a checksum.
- Wiring display-preference persistence only if the interface needs it.
- Leaving the admin dashboard out.
- This document is written but not committed; the tree has unrelated
  uncommitted work and nothing is committed without being asked.
