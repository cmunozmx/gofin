# gofin — phase 0/1

Jellyfin-wire-compatible media server. This tree is the *identity and protocol*
layer: the parts where being wrong locks out every client at once, and which
need no dependencies to get right.

It builds, vets, and runs. The original draft was written without a Go
toolchain; the only thing actually wrong with it was the file layout — every
source file sat flat in the repo root while declaring the package it was
*meant* to live in. Moving each into place was the whole fix. No logic changed.

## Build and run

```sh
CGO_ENABLED=0 go build ./cmd/gofin
go test ./...

./gofin config > gofin.json     # write a default config
# add a user to gofin.json:
#   "users": [{"name": "admin", "password": "changeme", "admin": true}]
./gofin serve --log-level debug
```

Verify it is alive the way a client would:

```sh
curl -s localhost:8096/System/Info/Public | jq
curl -s -X POST localhost:8096/Users/AuthenticateByName \
  -H 'Content-Type: application/json' \
  -H 'Authorization: MediaBrowser Client="curl", Device="shell", DeviceId="dev1", Version="1.0"' \
  -d '{"Username":"admin","Pw":"changeme"}' | jq
```

Then point jellyfin-web at it. It will reach the login screen, authenticate,
open the WebSocket, and then fail on `/UserViews` with a 501 — which is exactly
the intended state. Every unimplemented route logs itself at WARN, so a browse
attempt produces a prioritized to-do list.

All of the above has been run against the built binary: `/System/Info/Public`
and the login round-trip answer as shown, `/Users/Me`, `/System/Info`,
`/System/Endpoint` and `/Sessions` return 200 with a token and 401 without,
`/UserViews` and `/Items` return 501, and `/socket` completes the RFC 6455
handshake, sends `ForceKeepAlive`, and answers `KeepAlive`.

## Tests

```sh
go test ./...
go test -race ./...     # the WebSocket write path is concurrent; this matters
```

93 test functions, 185 cases with subtests, all passing and race-clean.

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
| `jftime` — .NET ticks, ISO-8601 with 7 fractional digits | complete |
| `jfws` — RFC 6455 server WebSocket, stdlib only | complete |
| Config: defaults → file → env → flags | complete |
| PBKDF2-SHA512 password hashing and verification | complete |
| `/System/Info(/Public)`, `/System/Ping`, `/System/Endpoint`, `/Branding/*` | complete |
| `/Users/AuthenticateByName`, `/Users/Me`, `/Users/Public` | complete |
| `/socket` handshake + `ForceKeepAlive` + `KeepAlive` | complete |
| Subscription tickers (`Sessions`, `ScheduledTasksInfo`) | TODO, marked |
| Persistence — everything is in memory and dies on restart | TODO |
| Library, items, DTOs, playback, transcoding, metadata | not started |

## The one test that decides whether migration works

`internal/jfid/guid_test.go:TestKnownItemIDs` is skipped until you paste in real
values from a Jellyfin database:

```sh
sqlite3 /var/lib/jellyfin/data/library.db \
  "select hex(guid), Path, type from BaseItems where Path is not null limit 5;"
```

If those reproduce, an existing library keeps every item ID, and client caches,
favourites, playlists and deep links all survive the swap. If they do not, the
key construction in `NewItemID` is wrong and needs checking against
`LibraryManager.GetNewItemId` in the upstream source. I flagged the two things
I am genuinely unsure about in comments there — the string encoding
(UTF-16LE vs UTF-8) and the exact path normalization. Both are parameters, not
assumptions baked into the code.

## Next, in order

1. Swap `Store` for SQLite (`modernc.org/sqlite`, pure Go, keeps the static build).
2. Generate the remaining ~340 handlers from a real server's
   `/api-docs/openapi.json` with `oapi-codegen`, so the compiler tracks what is left.
3. Port `Emby.Naming` — self-contained, has an existing upstream test corpus,
   and blocks the scanner.
4. Scanner, item repository, query builder.
5. `DtoService`. Build the differential proxy before writing it, not after.
