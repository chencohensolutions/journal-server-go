# Journal server (Go)

GraphQL server (gqlgen) with mTLS for a private photo journal; users and devices are identified by client
certificate. Enrollment (one-time codes, certificate issuing, device registry, admin API) is built: see
[docs/authentication.md](docs/authentication.md).

The clients and the overall design live in `../journal-client-kmp`. **Read its
[docs/design.md](../journal-client-kmp/docs/design.md)** before server work: it records the agreed data format
(plain files as the truth, a portable data folder, sha256 ids, story folders named by UTC creation time) and the
upload and sync design agreed with the owner. **Media upload is built** (package `media`, per-user `data_folder`,
resumable `PUT /upload/{sha256}`, `GET /media/{sha256}` with Range, `GET /media/{sha256}/preview` via libvips): see
[docs/media.md](docs/media.md). Video posters, the change feed for stories and notes, and the story trash are not
built yet.

**The server targets macOS for now** (owner, Oct 20, 2026; libvips from Homebrew). Windows comes later, so keep it
easy: no cgo, Windows-safe file names (compared ignoring case), image/video tools run as separate programs. To run
on Windows meanwhile, `go run ./scripts/bundle-vips` puts a portable libvips that reads HEIC in `vips/` (gitignored).

`.github/copilot-instructions.md` predates the KMP client (it still says "journal-client-android").
