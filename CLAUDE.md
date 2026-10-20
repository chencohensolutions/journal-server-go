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
the user's threshold); run by hand for now. The change
feed for stories and notes, and the story trash, are not built yet.

**The server targets macOS for now** (owner, Oct 20, 2026; libvips from Homebrew). Windows comes later, so keep it
easy: no cgo, Windows-safe file names (compared ignoring case), image/video tools run as separate programs. To run
on Windows meanwhile, `go run ./scripts/bundle-vips` puts a portable libvips that reads HEIC and JPEG XL (Samsung DNG 1.7) in `vips/` (gitignored).

`.github/copilot-instructions.md` predates the KMP client (it still says "journal-client-android").
