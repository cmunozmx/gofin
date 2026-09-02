# gofin — phase 0/1

Jellyfin-wire-compatible media server. This tree is the *identity and protocol*
layer: the parts where being wrong locks out every client at once.

It has exactly one dependency, `modernc.org/sqlite`, added when persistence
landed. Everything else is the standard library. That driver is pure Go
specifically so `CGO_ENABLED=0` still produces a static binary and `make dist`
stays a single cross-compile.

It builds, vets, and runs. The original draft was written without a Go
toolchain; the only thing actually wrong with it was the file layout — every
source file sat flat in the repo root while declaring the package it was
*meant* to live in. Moving each into place was the whole fix. No logic changed.

## The upstream source is checked out locally

`/var/home/deanvlue/dev/jellyfin` — **read it from there, not from github.com.**
It is faster, it works offline, and it pins one known revision instead of
whatever `master` happens to be today.

```sh
JF=/var/home/deanvlue/dev/jellyfin
git -C "$JF" describe --tags        # v12.0-rc5-122-g422b2bb3d9
```

The parts this port draws on:

| Path under `$JF` | What is there |
|---|---|
| `Emby.Naming/Common/NamingOptions.cs` | extension tables, CleanDateTimes, CleanStrings, stacking rules |
| `Emby.Naming/Video/` | `VideoResolver`, `CleanDateTimeParser`, `CleanStringParser`, `StackResolver` |
| `Emby.Server.Implementations/Library/IgnorePatterns.cs` | what a scan skips |
| `src/Jellyfin.Database/` | EF Core provider abstraction and the SQLite provider |
| `MediaBrowser.Providers/Plugins/Tmdb/` | poster fetching |

Every table in `internal/jfnaming` was diffed against this checkout, not
transcribed from memory: the video and audio extension lists match exactly (53
and 81 entries), and the CleanDateTimes expressions are byte-identical to the
ones quoted in the Go comments.

Note the version. This checkout is **12.0-rc5**, while gofin reports itself as
`10.11.0` to clients — a deliberately conservative number, since clients gate
features on it.

## Build and run

```sh
CGO_ENABLED=0 go build ./cmd/gofin
go test ./...

cp gofin.example.json gofin.json   # annotated starting point; edit it
./gofin serve --dry-run            # print the resolved config, start nothing
./gofin serve --log-level debug
```

`gofin.example.json` documents every key inline and is the intended way in.
`./gofin config` prints the same defaults without the commentary.

## Running as a Podman Quadlet service

A `Dockerfile` is included to containerize gofin. Build and run it as a systemd-integrated service using Podman Quadlets:

```sh
# Build the container image
podman build -t localhost/gofin:latest .

# Create config and state directories
mkdir -p ~/.config/gofin ~/.local/share/gofin/{data,cache,logs,config}

# Copy configuration (edit as needed)
cp gofin.example.json ~/.config/gofin/gofin.json

# Install the Quadlet service file
mkdir -p ~/.config/containers/systemd
cp gofin.container.example ~/.config/containers/systemd/gofin.container
# Edit ~/.config/containers/systemd/gofin.container to adjust paths as needed

# Start the service
systemctl --user daemon-reload
systemctl --user start gofin

# Manage the service
systemctl --user status gofin                # Check status
systemctl --user logs gofin -f               # Tail logs
systemctl --user restart gofin               # Restart
systemctl --user stop gofin                  # Stop
```

**Key features of the Quadlet setup:**
- **Read-only media volumes** mounted from `/mnt/alberca` with `:Z` SELinux relabeling
- **User namespace mapping** (`UserNS=keep-id`) so the container user has proper access to volumes
- **Persistent state** in `~/.local/share/gofin/` for data, cache, logs, and config
- **Configuration** via `~/.config/gofin/gofin.json`
- **Port 8096** published for client access

The service restarts automatically on failure and integrates with systemd's user session.

## Configuring libraries

A library is one media root. Declare them in `gofin.json`:

```json
"libraries": [
  {"name": "Movies", "path": "/mnt/alberca/Movies", "type": "movies", "readonly": true},
  {"name": "Music",  "path": "/mnt/alberca/Music",  "type": "music",  "readonly": true}
]
```

or on the command line, repeating `--library`:

```sh
./gofin serve \
  --library 'name=Movies,type=movies,path=/mnt/alberca/Movies,readonly' \
  --library 'name=Music,type=music,path=/mnt/alberca/Music,readonly'
```

or through the environment as the same JSON, for containers:

```sh
GOFIN_LIBRARIES='[{"name":"Movies","path":"/mnt/alberca/Movies","type":"movies"}]'
```

`type` is a Jellyfin CollectionType: `movies`, `tvshows`, `music`,
`musicvideos`, `homevideos`, `boxsets`, `books`, or omitted for mixed content.
`name` defaults to the directory name.

**`readonly` defaults to true when omitted.** The unsafe value should not be
what you get by saying nothing, and a bool's zero value would otherwise make an
absent field mean *writable* — so the field is a pointer internally and unset
means read-only. Nothing in this build writes to media at all, so today it is a
statement of intent that becomes load-bearing once deletion and metadata
writeback exist. For a guarantee rather than an intent, mount the media
read-only too: a `bind,ro` mount is enforced by the kernel, which no
application-level flag can match.

Configuration is rejected at startup, before the listener opens, if a path is
missing, is not a directory, or cannot be listed by the service user — all of
which otherwise present as a library that scans "successfully" and is empty:

```
level=ERROR msg="library configuration" err="library \"Movies\": stat /mnt/alberca/Movies: no such file or directory"
```

Two libraries may not overlap, since every file in the overlap would resolve
twice under two parents and two different item IDs:

```
config: library "Movies" (/mnt/alberca/Movies) overlaps library "All" (/mnt/alberca);
one media root may not contain another
```

Configured libraries appear in a client and **browse**: `/UserViews` renders
them from configuration, and `/Items?ParentId=…` walks into real scanned media.
The scan runs in the background at startup — a large library would otherwise
look like a server that hangs — and `POST /Library/Refresh` triggers a rescan.

Playback works by direct play: `/Items/{id}/PlaybackInfo` answers how to play a
file and `/Videos/{id}/stream` serves the bytes. There is no transcoding, so a
client that cannot decode a given container is on its own.

`RunTimeTicks` is still absent from item listings — `ffprobe` is consulted for
`PlaybackInfo` and cached under `cache/probe/`, but `toItemDto` does not read
that cache, so durations do not reach a browse response.

Verify it is alive the way a client would:

```sh
curl -s localhost:8096/System/Info/Public | jq
curl -s -X POST localhost:8096/Users/AuthenticateByName \
  -H 'Content-Type: application/json' \
  -H 'Authorization: MediaBrowser Client="curl", Device="shell", DeviceId="dev1", Version="1.0"' \
  -d '{"Username":"admin","Pw":"changeme"}' | jq
```

Then point a real client at it. It reaches the login screen, authenticates,
opens the WebSocket, lists the configured libraries, and browses into them.
Every unimplemented route still logs itself at WARN, so a browse attempt
produces a prioritized to-do list.

All of the above has been run against the built binary: `/System/Info/Public`
and the login round-trip answer as shown, `/Users/Me`, `/System/Info`,
`/System/Endpoint` and `/Sessions` return 200 with a token and 401 without,
`/socket` completes the RFC 6455 handshake, sends `ForceKeepAlive` and answers
`KeepAlive`, and the four-request sequence a real Delfin client issues on
connect — avatar, views, display preferences, items — completes with no WARN
in the log.

### What a real client taught us

A Delfin session produced one finding worth recording. The server emits GUIDs
dashless, as Jellyfin does — but the client sends them **back dashed**:

```
we emit:      bdfc15bd7affae0cc62f831e3f8cc547
Delfin asks:  /Users/bdfc15bd-7aff-ae0c-c62f-831e3f8cc547/Views
```

Every route with a GUID in its path has to accept both spellings or it 403s
against its own token holder. `jfid.Parse` already did, which is the only
reason this worked first try — and there is now a test pinning it, because the
next such route will be written by someone who has not seen this log.

That session also showed which route forms matter: Delfin uses the legacy
per-user paths (`/Users/{id}/Views`), not the newer `/UserViews?userId=`.
Both are implemented; neither is safe to skip.

## Tests

```sh
go test ./...
go test -race ./...     # the WebSocket write path is concurrent; this matters
```

272 test functions, 458 cases with subtests, all passing and race-clean.

Where a value could be checked against something other than this code, it is.
The PBKDF2-SHA512 known answers and the stored-hash fixtures come from Python's
`hashlib`, so `pbkdf2SHA512` is checked against a reference implementation
rather than against itself, and `verifyPassword` is shown to accept hashes an
entirely different implementation produced — which is what matters when
importing an existing user table. The WebSocket accept keys are computed the
same way, and the first is the example from RFC 6455 §1.3. The `.NET` tick
epoch offset is checked against Python's `datetime`.

The WebSocket tests drive a real socket — `httptest.NewRecorder` cannot be
hijacked — with a hand-rolled client that masks its frames, so framing,
continuations, ping/pong, close, and all three payload-length encodings are
exercised across their boundaries (125/126/65535/65536 bytes) rather than
asserted about.

What the suite deliberately pins, because these are the things that break every
client at once: dashless GUIDs on the wire, empty policy arrays serializing as
`[]` and never `null`, `LastLoginDate` as `null` rather than the year 1, 401 vs
403 (a client re-prompts for login on one and merely hides a feature on the
other), and identical responses for a bad password and a missing user.

## What is actually implemented

| Area | State |
|---|---|
| `jfid` — dashless GUIDs, .NET byte order, deterministic item IDs | complete, tested |
| `jfauth` — the full authorization grammar and precedence chain | complete, tested |
| `jftime` — .NET ticks, ISO-8601 with 7 fractional digits | complete, tested |
| `jfws` — RFC 6455 server WebSocket, stdlib only | complete, tested |
| Config: defaults → file → env → flags | complete, tested |
| Library declaration + validation (path, type, readonly, overlap) | complete, tested |
| PBKDF2-SHA512 password hashing and verification | complete, tested |
| `/System/Info(/Public)`, `/System/Ping`, `/System/Endpoint`, `/Branding/*` | complete, tested |
| `/Users/AuthenticateByName`, `/Users/Me`, `/Users/Public` | complete, tested |
| `/socket` handshake + `ForceKeepAlive` + `KeepAlive` | complete, tested |
| `/UserViews`, `/Users/{id}/Views` — libraries appear in clients | complete, tested |
| `/DisplayPreferences/{id}` — defaults; writes still discarded | partial, tested |
| Subscription tickers (`Sessions`, `ScheduledTasksInfo`) | TODO, marked |
| `jfnaming` — Emby.Naming port: titles, years, stacks, extras | complete, tested |
| `jfscan` — movie + music scanner, ignore rules, symlink-loop safe | complete, tested |
| `/Users/{id}/Items`, `/Items/{id}` — real browsing with paging | complete, tested |
| Item repository — in memory, atomic swap on rescan | complete, tested |
| `/Items/Latest`, `/Items/Resume`, `/Items/Root` | complete, tested |
| `/Items/{id}/Images/Primary` — local artwork, or fetched posters | complete, tested |
| `jftmdb` — TMDB poster fetching, opt-in, own API key | complete, tested |
| `jfdb` — SQLite: users, tokens, user data, display prefs | complete, tested |
| Tokens and logins survive a restart | complete, tested |
| Favourites, watched state, resume points | complete, tested |
| `/Sessions/Logout`, `DELETE /Sessions/AccessToken` | complete, tested |
| Display preferences — table exists, handlers not wired | not started |
| Item repository in SQLite — still in memory, rescan on boot | not started |
| Playback: direct play + byte-range streaming | complete, tested |
| Transcoding, metadata providers beyond TMDB, TV episodes | not started |

## Porting Emby.Naming: why it is not a regex translation

`internal/jfnaming` is a port of [Emby.Naming][], the library that turns a path
into a title and a year. Upstream is built almost entirely out of .NET regular
expressions, and **none of the load-bearing ones compile in Go**. RE2 has no
backtracking, so no lookahead and no lookbehind:

```
(?![0-9]+|\W[0-9]{2}\W[0-9]{2})   in every CleanDateTime expression
(?<=[\]\)\}])                     in both stacking rules
```

`regexp.MustCompile` *panics* on these, so a copy-paste port does not merely
misbehave — it fails to start. Each function therefore reproduces the semantics
by hand, with the upstream expression quoted directly above it so the two can
be diffed by eye when upstream changes.

One ordering detail had to be reproduced deliberately. CleanDateTime's leading
`(.+)` is greedy, which means the **rightmost** qualifying year wins. Scan left
to right and `2001 A Space Odyssey (1968)` becomes a film called "" made in
2001. There is a test pinning it.

The port is faithful including where upstream is odd. `Movie - (2016)` resolves
to the title `Movie -`, dash intact, because a space is not in upstream's
excluded character class. Quietly "fixing" that would diverge item names from
Jellyfin's, which is the one thing a compatibility layer must not do.

[Emby.Naming]: https://github.com/jellyfin/jellyfin/tree/master/Emby.Naming

## What a real library taught the scanner

Run first against a real 66-film library, the scanner produced **97 movies** —
and the surplus was not random:

| Symptom | Cause | Fix |
|---|---|---|
| 29 films called `Featurettes` | extras subdirectories scanned as film folders | skip upstream's extras-folder vocabulary |
| `ONCE` twice | `ONCE_Trailer.mkv` beside the feature took the folder's name | drop files matching the extra-suffix rule |
| `Star Wars Trilogy` three times | 3 distinct films in one folder, each given the folder's name | a folder names its film only when it holds exactly one |

After the fix: 66 items, no duplicate titles, a year parsed for 61 of them.
Each of the three has a regression test.

Names are only as good as the file layout, because nothing here reads container
metadata or contacts a provider — `Blade.Runner (1997)` resolves to
`Blade.Runner`, dots and all. That is also what Jellyfin shows before its
metadata providers run; providers are what replace the title, and there are
none yet.

## Posters

Local artwork is used when present: the conventional names only — `poster`,
`folder`, `cover`, `default`, `movie`, `front`, `album`, any case, `.jpg`
`.jpeg` `.png` `.webp`. Deliberately *not* "any image in the folder", because
release groups drop advertising banners beside the media and showing one of
those as a film's poster is worse than showing none. (The library this was
built against has 66 films and **zero** conventional posters — every JPEG in it
is a `WWW.YTS.AG.jpg` advert.)

For everything else there is TMDB, which is what Jellyfin uses
(`$JF/MediaBrowser.Providers/Plugins/Tmdb/`). It is **off by default** and needs
two things to turn on:

```json
"metadata": {
  "tmdb": {
    "enabled": true,
    "api_key": "YOUR_OWN_KEY",
    "language": "en-US",
    "poster_size": "w500"
  }
}
```

or keep the credential out of the file entirely — setting the environment
variable also enables the provider:

```sh
GOFIN_TMDB_API_KEY=... ./gofin serve
```

**Use your own key.** Jellyfin ships a hardcoded one in `TmdbUtils.cs` and it is
deliberately not reused here: that key belongs to the Jellyfin project, TMDB's
terms require an application to use its own, and borrowing it would risk
getting *their* key rate-limited by our traffic. A key is free
([signup](https://www.themoviedb.org/signup) →
[API settings](https://www.themoviedb.org/settings/api)), and enabling without
one logs a warning and stays off rather than failing quietly.

How it behaves, and why:

- **Posters are written to `<cache>/images/`, never beside your media.** That is
  what keeps `readonly: true` honest. Deleting the directory costs only
  bandwidth.
- **The cache is checked before the network**, at scan time — so a restart shows
  artwork immediately. This was the first thing gofin persisted across restarts,
  and it predates the database: posters are files on disk, not rows.
- **Unmatched titles get a `.miss` marker**, because a library of scene releases
  has plenty and re-querying them on every boot is pure waste.
  `POST /Library/RefreshArtwork?retryMissed=true` clears them, which is what you
  want after renaming the offending files.
- **A 401 or 429 stops the whole run.** Both fail identically for every
  remaining item, so continuing would send one doomed request per film — the
  exact behaviour that gets a key banned.
- **Requests are throttled** (~60ms apart). A first scan of a large library is
  precisely the traffic shape that trips rate limiting.
- `poster_size` defaults to `w500`, not upstream's `original`: `original` means
  downloading several megabytes per film to render a thumbnail a few hundred
  pixels wide.
- Only movies are looked up. Music would need MusicBrainz, which is not ported.

The tests run against a fake TMDB served by `httptest`, so `go test` never
touches the network and needs no key.

## Persistence

There is a database: `data/gofin.db`, opened at startup by `internal/jfdb`.
Four tables at `PRAGMA user_version` 1 — `users`, `devices`, `user_data`,
`display_preferences`. WAL, `synchronous=NORMAL`, foreign keys on, and a pool
capped at one connection so statements serialise and `SQLITE_BUSY` cannot
happen. That pool size is also why every query defers `rows.Close()`: a leaked
`rows` deadlocks the next caller rather than merely leaking.

`--dry-run` returns before the database is opened, so printing the resolved
config never creates the file as a side effect.

What now survives a restart:

- **Tokens.** Clients stay logged in. Because they outlive the process, there
  is now a way to end one deliberately: `POST /Sessions/Logout`, and
  `DELETE /Sessions/AccessToken` for an admin ending somebody else's session.
- **Favourites, watched state and play counts**, via the `FavoriteItems` and
  `PlayedItems` routes in both their spellings.
- **Resume points.** The four `/Sessions/Playing*` routes used to read their
  bodies into `io.Discard`; those bodies carry `ItemId` and `PositionTicks`,
  which is the entire input for `/Items/Resume`. Stopping past 90% of an item's
  runtime marks it played and clears the resume point so it leaves Continue
  Watching.

What still does not:

- **Display preferences.** `POST /DisplayPreferences` still discards its body
  and `GET` still serves hardcoded defaults, even though the table and its
  repository methods exist and are tested. This is the last `io.Discard` sink.
- **The item store.** `ItemStore` is still in memory, so every boot re-scans —
  about 3s cold, 12ms once the directory entries are in page cache. It is
  deliberately shaped for the swap: replaced wholesale at the end of a scan, so
  already atomic, and reached only through `Get`, `Children`, `Descendants` and
  `ChildCount`. That was left for later on purpose — a rescan is cheap, so the
  item table buys much less than the user-data table and costs considerably
  more, since it touches the atomic-swap semantics that keep a rescan invisible
  to clients.

### Users are declared in config, cached in the database

`gofin.json` remains the source of truth for who exists; `seedUsers` upserts
them on every boot. There is no user CRUD over HTTP, so a user is created,
renamed or promoted by editing the config.

Earlier versions kept the PBKDF2 salt stable by **rewriting `gofin.json`** with
the computed hash. That is gone. It wrote to `defaultConfigPath()` rather than
the `-config` path, so `-config other.json` silently edited `gofin.json`.
The rule now: an explicit `password_hash` wins; otherwise a stored hash that
still verifies against the configured plaintext is kept; otherwise the plaintext
is hashed. Same stable salt, nothing written to your config file.

Note `config/` and `logs/` are still created and still stay empty — the config
file lives at `./gofin.json` (or `$GOFIN_CONFIG`), not inside `cfg.Paths.Config`.

## Caching

There is none, and that is worth stating plainly because it looks like a bug in
a log full of repeated requests:

- **No HTTP cache headers** on any JSON response — no `Cache-Control`, no
  `ETag`. Clients re-request everything, every time.
- Artwork is the single exception: it is immutable once written, so it goes out
  with a one-year `max-age` and an `ETag` derived the way Jellyfin derives image
  tags.
- A poster requested twice in the same millisecond is two UI elements asking for
  the same file, not a retry loop.

## The one test that decides whether migration works

`internal/jfid/guid_test.go:TestKnownItemIDs` is skipped until you paste in real
values from a Jellyfin database. **Which query to run depends on the version.**
Jellyfin 10.10 folded `library.db` into the EF Core database and changed how a
GUID is stored, so the two eras need different commands:

**10.10 and newer** — one `jellyfin.db`, GUIDs stored as `TEXT`:

```sh
sqlite3 /var/lib/jellyfin/data/jellyfin.db \
  "select Id, Path, Type from BaseItems where Path is not null limit 5;"
```

`Id` comes back as an ordinary dashed GUID — paste it in as-is. Note the
capitalised `Id` and `Type`: EF Core renamed both columns.

**10.9 and older** — raw SQLite in `library.db`, GUIDs as a 16-byte BLOB in
.NET byte order:

```sh
sqlite3 /var/lib/jellyfin/data/library.db \
  "select hex(guid), Path, type from BaseItems where Path is not null limit 5;"
```

That one is byte-swapped and has to go through `FromDotNetHex`.

If those reproduce, an existing library keeps every item ID, and client caches,
favourites, playlists and deep links all survive the swap. If they do not, the
key construction in `NewItemID` is wrong and needs checking against
`LibraryManager.GetNewItemId` in the upstream source. I flagged the two things
I am genuinely unsure about in comments there — the string encoding
(UTF-16LE vs UTF-8) and the exact path normalization. Both are parameters, not
assumptions baked into the code.

## What upstream actually uses for storage

Verified against `jellyfin/jellyfin` master rather than recalled, because it
decides what step 1 of the roadmap should look like:

- **Yes, SQLite** — it is the default and currently the only supported provider.
- But it is no longer raw SQLite. Since 10.10 it is **EF Core**, behind a
  provider abstraction at `src/Jellyfin.Database/`, with
  `Jellyfin.Database.Providers.Sqlite` the only implementation so far. Upstream's
  own readme there says PostgreSQL is planned as an experimental second provider.
- **One database file, `jellyfin.db`.** The old split (`library.db`,
  `activitylog.db`) was consolidated by the `20241020103111_LibraryDbMigration`
  migration.
- `BaseItems` is now a `DbSet` on `JellyfinDbContext`, and the model snapshot
  declares `b.Property<Guid>("Id").HasColumnType("TEXT")` — item GUIDs are
  stored as dashed strings, not byte-swapped blobs.

The practical consequence for this port: `FromDotNetBytes` is still needed, because
the byte swap still happens inside .NET when the MD5 digest is turned into a
`Guid` during derivation. But `FromDotNetHex` is only for reading a pre-10.10
`library.db`; pointed at a modern database it would swap a value that is already
in the right order.

Storage is a private implementation detail — nothing about it is on the wire —
so gofin does not have to match it. Matching the *derivation* is what preserves
item IDs, and that is what `TestKnownItemIDs` checks.

## One bug the tests surfaced, still open

**`jftime` is not on the timestamp path.** `internal/jftime` exists specifically
so timestamps render the way .NET's serializer does — ISO-8601 with exactly
seven fractional digits — and its own comment notes that some clients
string-compare timestamps.

`main.go` does now import it, but only for `jfid.ImageTag`. The DTOs still
declare `*time.Time` / `time.Time`, so the live server emits Go's default
nanosecond precision:

```
"LastActivityDate": "2026-08-24T15:47:46.916119508Z"   // 9 digits, not 7
```

The fix is to change `LastLoginDate`, `LastActivityDate` and friends to
`jftime.Time`. Small, but it touches every timestamp on the wire, so do it
before anything starts depending on the current shape. `jftime` itself is
correct and tested — nothing is wrong except that the DTOs do not use it.

**Fixed:** `problem()` used to compute the RFC 9110 section as `status-399`,
right for 4xx and nonsense for everything else — a 501 advertised
`section-15.5.102`, which does not exist. Status now maps to section explicitly.

## Next, in order

0. Fix the `jftime` bug above — cheap now, more expensive once anything depends
   on the current wire shape.
1. **Wire display preferences.** The table and its four repository methods are
   built and tested; two handlers need to read and write them, keyed on
   `(user, prefs_id, client)`. The last `io.Discard` sink in the tree.
2. **Handler tests for the new routes.** `internal/jfdb` has 43 of its own, but
   favourites, played, resume, logout and the playback reporters have no
   coverage at the HTTP layer. The one worth writing first is a round trip:
   report progress, restart, confirm `/Items/Resume` returns the item.
3. `RunTimeTicks` in item listings. `ffprobe` output is already cached under
   `cache/probe/`; `toItemDto` just does not read it. Durations are what make a
   progress bar work in a browse view.
4. Generate the remaining handlers from a real server's
   `/api-docs/openapi.json` with `oapi-codegen`, so the compiler tracks what is
   left. `$JF` can produce the spec without a running server.
5. TV: port the episode expressions from
   `$JF/Emby.Naming/Common/NamingOptions.cs` (`EpisodeExpressions`) and the
   `Emby.Naming/TV/` resolvers. `jfscan` currently treats a `tvshows` library as
   loose video files, which is honest but not useful.
6. The WebSocket subscription tickers, so a client's dashboard stops waiting
   forever for `ScheduledTasksInfo` that never arrives. `GET /Sessions` also
   still returns `[]` — there is no session registry, and `handleSocket` keeps
   its connection on the goroutine stack rather than in one.
7. The item store in SQLite, with an ancestors closure table so boot stops
   re-scanning and `Descendants` becomes a join.

Done since this list was written: `Emby.Naming`, the scanner and item
repository, library browsing, direct-play streaming, and SQLite persistence for
users, tokens and user data.
