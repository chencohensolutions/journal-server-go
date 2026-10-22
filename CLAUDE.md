# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Journal server (Go)

GraphQL server (gqlgen) with mTLS for a private photo journal; users and devices are identified by client
certificate. Enrollment (one-time codes, certificate issuing, device registry, admin API) is built: see
[docs/authentication.md](docs/authentication.md).

The clients and the overall design live in `../journal-client-kmp`. **Read its
[docs/design.md](../journal-client-kmp/docs/design.md)** before server work: it records the agreed data format
(plain files as the truth, a portable data folder, sha256 ids, story folders named by UTC creation time) and the
upload and sync design agreed with the owner. **Media upload is built** (package `media`, per-user `data_folder`,
resumable `PUT /upload/{sha256}`, `GET /media/{sha256}` with Range, `GET /media/{sha256}/preview` via libvips, `POST /media/{sha256}/verify` re-hashes an original before a device
deletes its copy): see
[docs/media.md](docs/media.md). Video posters are made with ffmpeg (a separate program, like libvips). `go run ./scripts/import` copies photos
and videos already on the server's disk into a data folder once each (by sha256) and moves the sources to an
"imported" folder. The server, imports and scans may run at once: they take turns at writing to the data folder
(`media/turns.go`, lock and journal in `.cache/`), so any new code that places files or rewrites sidecars must do
it inside `Store.Turn` (the store's own write methods already do). `go run ./scripts/scan-nudity` scores files with
NudeNet (Python, a separate program) into the sidecar's `detected.nudity` (never a tag; clients hide what is over
the user's threshold); run by hand for now. Users mark files private with `setPrivate` (sidecar `privacy`,
`media/private.go`); apps hide them like nudity, and devices set to withhold get them as deleted and 404. **Text
search** (`searchMedia`, package `search`, [docs/search.md](docs/search.md)) runs SigLIP 2 with ONNX Runtime inside
the server (loaded with purego, no cgo, no Python): files are read into vectors in the background, kept in
`.cache/search/`. The change feed for stories and notes, and the story trash, are not built yet.

**The server targets macOS for now** (owner, Oct 20, 2026; libvips from Homebrew). Windows comes later, so keep it
easy: no cgo, Windows-safe file names (compared ignoring case), image/video tools run as separate programs. To run
on Windows meanwhile, `go run ./scripts/bundle-vips` puts a portable libvips that reads HEIC and JPEG XL (Samsung DNG 1.7) in `vips/` (gitignored).

`.github/copilot-instructions.md` predates the KMP client (it still says "journal-client-android").

## Commands

```bash
go run .                                   # serve https://0.0.0.0:56398 (needs config.json + certs/)
go build -o journal-server .               # what the VS Code task "server: run" does
go test ./...                              # ~8 s; previews/poster/search tests skip without libvips/ffmpeg/model
go run ./scripts/bundle-onnx [--model large] [--precision q8]   # ONNX Runtime + search model (gitignored)
go test ./media -run TestTurns             # one test
go tool gqlgen generate                    # after editing graph/schema/*.graphql
go tool gqlgen generate --config gqlgen.admin.yml   # after editing graph/admin/schema/*.graphql
go run . enroll-code <user> | devices [user] | revoke <serial>   # admin commands; server must be running
go run . private <serial> withhold|allow   # keep the user's private files from a device (default: sent, flagged)
go run ./scripts/import --user <name> --imported <folder> --not-imported <folder> [--dry-run] <folder>...
go run ./scripts/scan-nudity --user <name> [--rescan]
go run ./scripts/fix-sources --user <name> [--dry-run]    # once: source=whatsapp for files WhatsApp named
```

Lint is golangci-lint on save in VS Code (gofmt, govet, staticcheck, errcheck, gosimple); there is no
`.golangci` file, so `go vet ./...` is the command-line check. `certs/`, `admin.token` and `devices.json` are gitignored local
state. `config.json` is committed and is the owner's real config (user names, data folders); `config.example.json`
shows the shape.
`scripts/generate-ca.sh` and `generate-server-cert.sh` make the local CA and server cert.

## Architecture

**Two GraphQL schemas, one binary.** `main.go` loads `config.json` (`auth.LoadConfig`, also used by the
scripts) and dispatches: no args serves, otherwise `admincli.go` runs an admin command as a GraphQL client of
the running server. `serve.go` is startup and the routing table; the middleware (`identify`, gzip, deadlines)
is in `middleware.go`, opening data folders and finding libvips/ffmpeg in `library.go`. The public schema (`graph/schema/`, generated into
`graph/generated`, resolvers in `graph/resolver/`) is on `:56398` with mTLS. The admin schema
(`graph/admin/`, `admin.go`) is plain HTTP on a loopback-only address with a bearer token the server rewrites
into `admin.token` on every start. Admin operations never appear on `/query`. Both are regenerated separately
(see commands); don't edit `*_gen.go` or `generated.go`.

**Access is deny by default.** TLS uses `VerifyClientCertIfGiven` so enrolling devices can connect without a
certificate. `identify` in `middleware.go` maps the cert CN to a `config.json` user and rejects revoked serials
(`enroll.Registry`, persisted in `devices.json`). `auth.RequireUserUnlessPublic` is a root-field middleware: an
anonymous request can only reach root fields marked `@public` in the schema (the two enrollment fields), and
introspection is blocked too. The media HTTP routes (`/upload`, `/media`) always require a
certificate. Resolvers get the user with `auth.FromContext(ctx)`; `Resolver.store(ctx)` returns that user's
`media.Store`.

**Package `media` is the data layer and owns the on-disk format.** `Library` maps usernames to `Store`s; a
`Store` is one user's `data_folder`, indexed into memory from the sidecar JSON files at start (`scan`). Files are
the truth: there is no database. Originals go under `media/<year>/<year-month>/`, sidecars sit beside them as
`<name>.json`, uploads in progress and previews live in `.cache/`. `index.go` is the start-up scan, which
reads only folders whose modified time differs from `.cache/index.json`. `names.go` makes file names safe on every
platform; `files.go` has the atomic write and rename helpers. `store.go` is the index and lookups, `upload.go`
the resumable upload, `feed.go` the change feed, `library.go` the per-user map, `sidecar.go` the sidecar format
and the only code that rewrites one (`rewriteSidecar`: folders from imports, nudity scores, corrected sources;
a rewrite clients must see sets a time that `Sidecar.ChangedAt` counts). `source.go` tells a file's source from
its path and name, for imports and `scripts/fix-sources`. HTTP handlers for
upload/original/preview/verify are in `media/http.go` and `verify.go`, mounted by `newMux` in `serve.go`;
resolvers in `graph/resolver/media.resolvers.go` cover `missingMedia`, `startUpload`, the change feed and usage.
The change feed (`Store.Changes`) orders items by when their sidecar last changed, with an opaque cursor, so a
rewritten sidecar comes again and the cursor can later cover notes and stories. Previews: `previewspec.go` parses
and plans a request, `preview.go` runs libvips, `poster.go` ffmpeg, `dng.go`/`develop.go` develop Samsung's
JPEG XL DNGs that libvips can't.

**Multi-process writes go through turns** (`media/turns.go`). The server, one or more `scripts/import` runs and
`scripts/scan-nudity` may share a data folder. Each holds an exclusive file lock on `.cache/write.lock` only
for the milliseconds it takes to place a file or rewrite a sidecar, and appends to `.cache/write-journal.tsv`
so the others can catch up (`Store.CatchUp`, `OnCatchUp`). Copying and hashing happen before the turn. Anything
that writes into the data folder from outside the store's own methods must run inside `Store.Turn`. The file
lock and disk-free checks have per-OS files (`*_unix.go`, `*_windows.go`, `*_other.go`); keep that pattern
instead of cgo or build-tagged dependencies.

**External tools are separate programs, found at start.** `media.FindVips` / `FindFFmpeg` look on the PATH
(and `vips/bin` first), or at `vips` / `ffmpeg` in `config.json`. Without them the server still runs and the
affected endpoints answer 503. `scripts/scan-nudity` embeds a Python worker (`nudenet_worker.py`) and feeds it
640 px previews, never originals. The one library loaded into the server is ONNX Runtime (text search):
`search/ort.go` calls its C function table through purego by fixed places counted in `onnxruntime_c_api.h`, so
add a function by its place there; `search.FindRuntime` looks in `onnxruntime/`, then Homebrew. Without it or the
model, search is off and `searchMedia` answers an error. `scripts/import` is split by concern: `main.go` (flags and the run), `pipeline.go`
(walk, examine, decide), `facts.go` (dates, size, GPS from EXIF/ffprobe), `near.go` (dHash near-duplicates),
`outcomes.go` (moving files, progress, reports), `review.go` (the browser review page), `cloud.go` (Google
Drive / OneDrive through rclone).

## Conventions worth knowing

- Doc comments and docs are written in plain prose for the owner; keep [docs/media.md](docs/media.md),
  [docs/search.md](docs/search.md) and
  [docs/authentication.md](docs/authentication.md) in step with behaviour changes.
- The sidecar `format` is versioned; a new field is additive, and `detected` is for model output only,
  never user tags. File names from devices go through `safeFileName` (no path parts, reserved names, or
  `[...]`, which libvips reads as options); `mimeType` is validated before it is echoed in Content-Type.
- Tests use real temp data folders. `newTestEnv` in `serve_test.go` starts a full mTLS test server with a
  generated CA, enrolled device and `Library`; `media` tests drive a `Store` directly. Add tests that way
  rather than mocking the store.
