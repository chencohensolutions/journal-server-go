# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

# Journal server (Go)

GraphQL server (gqlgen) with mTLS for a private photo journal; users and devices are identified by client
certificate. The clients and the overall design live in `../journal-client-kmp` (the VS Code workspace opens both).
**Read its [docs/design.md](../journal-client-kmp/docs/design.md)** before server work: it records the agreed data
format (plain files as the truth, a portable data folder, sha256 ids, story folders named by UTC creation time) and
the upload and sync design agreed with the owner. The docs here describe what is built, and must be kept in step:

- [docs/authentication.md](docs/authentication.md): enrollment (one-time codes, certificate issuing, device
  registry), the admin API, revocation, withholding private files from a device.
- [docs/media.md](docs/media.md): the data folder and sidecar format, resumable `PUT /upload/{sha256}`,
  `GET /media/{sha256}` with Range, previews and video posters, `/verify`, the change feed, nudity scores, private
  files, hidden collections, imports, turns between processes.
- [docs/search.md](docs/search.md): text search (`searchMedia`) with SigLIP 2 on ONNX Runtime inside the server.
- [docs/faces.md](docs/faces.md): face search: YuNet and SFace on the same runtime, groups of alike faces, people
  the user names (`people.json`), `people`, `similarFaces`, `searchMedia(people:)`.
- [docs/indexes.md](docs/indexes.md): the reader that feeds every index from one decode of each original (nudity,
  faces, OCR, search, DINOv2 similar photos and duplicates, LAION aesthetic scores), the models and their costs,
  and how to add an index.

Not built yet: stories and notes in the change feed, the story trash, certificate renewal.

**Platforms.** The owner's server is a Windows PC (search runs there since Oct 22, 2026), development happens on a
Mac, and the owner plans to distribute the server to the community on Windows, macOS and Linux (Oct 21, 2026). So: no cgo, Windows-safe file names (compared ignoring
case), per-OS files for file locks, disk-free checks and loading libraries (`*_unix.go`, `*_windows.go`,
`*_other.go`), and ffmpeg runs as a separate program. Two libraries are loaded into the process through purego:
ONNX Runtime, and libvips (since Oct 23, 2026; `"vips_as_program": true` keeps libvips out).

## Commands

```bash
go run .                                   # serve https://0.0.0.0:56398 (needs config.json + certs/)
go build -o journal-server . && ./journal-server    # what the VS Code task "server: run" does
go test ./...                              # ~10 s; previews/poster/search/face tests skip without libvips/ffmpeg/models
go test ./media -run TestTurns             # one test
go test ./search/... .                     # the search package and the root (server) tests
go vet ./...                               # the command-line lint (VS Code runs golangci-lint on save; no .golangci file)
go tool gqlgen generate                    # after editing graph/schema/*.graphql
go tool gqlgen generate --config gqlgen.admin.yml   # after editing graph/admin/schema/*.graphql
go run ./scripts/bundle-onnx [--model large] [--precision q8]   # ONNX Runtime + every index's model into onnxruntime/, models/ (gitignored)
go run ./scripts/bundle-vips               # portable libvips for Windows into vips/ (gitignored)
go run . enroll-code <user> | devices [user] | revoke <serial>   # admin commands; the server must be running
go run . private <serial> withhold|allow   # keep the user's private files from a device (default: sent, flagged)
go run ./scripts/import --user <name> --imported <folder> --not-imported <folder> [--dry-run] <folder>...
go run ./scripts/scan-nudity --user <name> [--rescan]     # NudeNet on ONNX Runtime; the server scans new files itself
go run ./scripts/fix-sources --user <name> [--dry-run]    # once: source=whatsapp for files WhatsApp named, screenshots' app
```

- **After changing the public schema**, the client must get the copy: in `../journal-client-kmp` run
  `./gradlew :sharedLogic:syncServerSchema` and commit the `.graphqls` files there. After changing the CA, run
  `./gradlew :sharedLogic:syncServerCa` there and ship a new build.
- `certs/`, `admin.token` and `devices.json` are gitignored local state. `config.json` is committed and is the
  owner's real config (user names, data folders); `config.example.json` shows the shape. A second server for
  testing runs from its own folder with its own `config.json` (`port`), beside the real one.
- `scripts/generate-ca.sh` and `generate-server-cert.sh` (`.ps1` on Windows) make the local CA and server cert.
  The server cert's SANs are listed in the script; a device connecting by an address not listed rejects it.
- `.vscode/tasks.json` has "server: run", "server: enroll code", a Delve task for remote debugging on the owner's
  server PC (`launch.json` "Attach to server PC"), and adb tasks for the phone over a hotspot.

## Architecture

**Two GraphQL schemas, one binary.** `main.go` loads `config.json` (`auth.LoadConfig`, also used by the scripts)
and dispatches: no args serves, otherwise `admincli.go` runs an admin command as a GraphQL client of the running
server. `serve.go` is startup and the routing table (`newMux`); the middleware (`identify`, `hideFromDevice`,
gzip, deadlines) is in `middleware.go`; opening data folders and finding libvips/ffmpeg/ONNX Runtime in
`library.go`. The public schema (`graph/schema/*.graphql`, one file per area, generated into `graph/generated`,
resolvers in `graph/resolver/`) is on `:56398` with mTLS. The admin schema (`graph/admin/`, `admin.go`) is plain
HTTP on a loopback-only address with a bearer token the server rewrites into `admin.token` on every start. Admin
operations never appear on `/query`. `graph/generated` and `graph/model/*_gen.go` are gqlgen output (don't edit);
resolver bodies in `*.resolvers.go` are yours, and gqlgen carries them over.

**Access is deny by default.** TLS (`tlsConfigFor` in `serve.go`, shared with the tests) uses
`VerifyClientCertIfGiven` so enrolling devices can connect without a certificate, and offers HTTP/2 beside
HTTP/1.1 (Go sets h2 up only when `NextProtos` names it, because the listener is made from the config). `identify` maps the cert CN to a `config.json` user and rejects revoked serials (`enroll.Registry`,
persisted in `devices.json`). `auth.RequireUserUnlessPublic` is a root-field middleware: an anonymous request can
only reach root fields marked `@public` in the schema (the two enrollment fields), and introspection is blocked
too. The media HTTP routes (`/upload`, `/media`) always require a certificate. Resolvers get the user with
`auth.FromContext(ctx)`.

**Everything a device reads goes through a `media.View`.** A user can hide whole collections, and what the nudity
scan scored at a chosen score or more (from 0.5 by default), on every device (`setHiddenCollections`,
`settings.json` at the data folder's root, `media/hidden.go`) and mark files private
(`setPrivate`, sidecar `privacy`, `media/private.go`); a device can be set to withhold private files. The server
does the hiding: such files come in the change feed as deleted (and `hidden`, for what the user hides), `media(sha256:)` is null, `/media` and its preview
answer 404, usage and search leave them out. In resolvers use `Resolver.view(ctx)` (store plus the device's
`View`) for anything shown to a device, and `Resolver.store(ctx)` only for what must see every file
(`missingMedia`, `startUpload`, verify). Any new read path for devices must check `Store.Hides`. The feed cursor
carries the view's key, so changing either setting restarts that device's feed.

**Package `media` is the data layer and owns the on-disk format.** `Library` maps usernames to `Store`s; a
`Store` is one user's `data_folder`, indexed into memory from the sidecar JSON files at start (`scan`). Files are
the truth: there is no database. Originals go under `media/<year>/<year-month>/`, sidecars sit beside them as
`<name>.json`, uploads in progress and previews live in `.cache/`. `index.go` is the start-up scan, which reads only
folders whose modified time differs from `.cache/index.json`. `names.go` makes file names safe on every platform;
`files.go` has the atomic write and rename helpers. `store.go` is the index and lookups, `upload.go` the resumable
upload, `feed.go` the change feed (ordered by when a sidecar last changed; the opaque cursor carries the position and
the device's `View`, so a view change sends only the files whose visibility changed, an unreadable cursor restarts
the feed, and notes and stories can join later), `library.go` the per-user map, `sidecar.go` the
sidecar format and the only code that rewrites one (`rewriteSidecar`; a rewrite clients must see sets a time that
`Sidecar.ChangedAt` counts). `source.go` tells a file's source from its path and name. HTTP handlers for
upload/original/preview/verify are in `http.go` and `verify.go`. Previews: `previewspec.go` parses and plans a
request, `preview.go` makes previews with libvips (NumCPU/2 at once), inside the server through `vipslib*.go`
(purego, fixed-argument calls only: an operation built by name and its properties; `vips.exe` as the fallback for
what it fails on), `original.go` decodes an original once for the indexes and makes each model's exact picture
from it, `poster.go` ffmpeg, `dng.go`/`develop.go` develop Samsung's JPEG XL DNGs that libvips can't (in memory when
libvips is in the server). Previews are kept on disk only with the user's `preview_cache`; otherwise each is made, sent and deleted.
That default is the owner's choice (Oct 22, 2026): the server has several users, each with their own data folder,
and a user's phone keeps every preview it asks for and is usually the only device reading that folder, so a server
cache would hold copies of what the phone already has. Don't propose turning it on.

**Multi-process writes go through turns** (`media/turns.go`). The server, one or more `scripts/import` runs,
`scripts/scan-nudity` and `scripts/fix-sources` may share a data folder. Each holds an exclusive file lock on
`.cache/write.lock` only for the milliseconds it takes to place a file or rewrite a sidecar, and appends to
`.cache/write-journal.tsv` so the others can catch up (`Store.CatchUp`, `OnCatchUp`). Copying and hashing happen
before the turn. Anything that writes into the data folder from outside the store's own methods must run inside
`Store.Turn` (the store's own write methods already do). The exceptions are `.cache/search/`, `.cache/faces/` and
the indexes' caches (`.cache/ocr/`, `.cache/similar/`, `.cache/aesthetic/`) and failure lists, which only the
server writes.

**External tools are found at start.** `media.FindVips` / `FindFFmpeg` look on the PATH (and `vips/bin` first), or
at `vips` / `ffmpeg` in `config.json`; libvips's library is then loaded from beside `vips` (on Windows its folder is
searched first for DLLs, so another libvips on the PATH is never mixed in). Without them the server still runs and
the affected endpoints answer 503. No Python: the owner asked to avoid it (Oct 23, 2026). ONNX Runtime (package `onnx`, loaded once by `loadRuntime` in `library.go` and shared):
`ort.go` calls its C function table through purego by fixed places counted in `onnxruntime_c_api.h`, so add a
function by its place there; `onnx.FindRuntime` looks in `onnxruntime/`, then Homebrew. Text search (package
`search`) keeps vectors in `.cache/search/<model>.vectors`; face search (package `faces`) keeps the faces of each
file in `.cache/faces/<model>.faces`, and the people the user named in `people.json` (`media/people.go`); the nudity
scan (package `nudity`, NudeNet's 320n detector, the same steps as NudeNet's Python) writes each score into the
sidecar (`Store.SetNudity`), shared with `scripts/scan-nudity`. All three are `reading.Index`es of one
`reading.Reader` (`newReader` in `library.go`): it decodes each original once, gives every index exactly the picture
its model reads, in order (nudity, faces, search), prepares the next files while the models read this one, and
keeps each index's failure list (docs/indexes.md). Packages `ocr` (Hebrew and English text in photos, found by
`searchMedia`), `similar` (DINOv2: `similarMedia`, `duplicates`) and `aesthetic` (LAION on CLIP L/14:
`Media.aesthetic`) are indexes too; logs name indexes by their model (`Index.Model`). Without the runtime or a model, that index is off (search queries
answer an error). The resolver's `Library` field is the `media.Library` (`Media` is gqlgen's resolver for
`Media.faces`). `scripts/import` is split by concern: `main.go` (flags and the run), `pipeline.go` (walk, examine, decide),
`facts.go` (dates, size, GPS from EXIF/ffprobe), `near.go` (dHash near-duplicates), `outcomes.go` (moving files,
progress, reports), `review.go` (the browser review page), `cloud.go` (Google Drive / OneDrive through rclone).

## Conventions worth knowing

- Doc comments and docs are written in plain prose for the owner; keep the three docs in step with behaviour
  changes, and the GraphQL schema's descriptions too (the client's generated code carries them).
- The sidecar `format` is versioned; a new field is additive, and `detected` is for model output only, never user
  choices (`privacy` is the user's, so it sits beside `facts`). File names from devices go through `safeFileName`
  (no path parts, reserved names, or `[...]`, which libvips reads as options); `mimeType` is validated before it is
  echoed in Content-Type.
- Results a device must act on are unions with a reason (`StartUploadResult`, `EnrollDeviceResult`), not errors.
  Errors a device acts on carry `extensions.code` (`graph/resolver/errors.go`, `coded(...)`; listed in
  docs/media.md "Errors"); anything else is a plain error for the log. `serverInfo` tells apps what the server can
  do (search, previews, posters, `api` version), so add to it rather than letting apps learn from errors.
- Tests use real temp data folders. `newTestEnv` in `serve_test.go` starts a full mTLS test server with a generated
  CA, enrolled device and `Library`; the root `serve_*_test.go` files cover one area each (media, search, faces,
  private, hidden) through that server (face tests write made-up faces and run a second server on the same folder
  with `fakeFaceModel`), and `media` tests drive a `Store` directly. Add tests that way rather than mocking
  the store.
