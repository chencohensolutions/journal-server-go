# Journal server (Go)

GraphQL server (gqlgen) with mTLS for a private photo journal; users and devices are identified by client
certificate. Enrollment (one-time codes, certificate issuing, device registry, admin API) is built: see
[docs/authentication.md](docs/authentication.md).

The clients and the overall design live in `../journal-client-kmp`. **Read its
[docs/design.md](../journal-client-kmp/docs/design.md)** before server work: it records the agreed data format
(plain files as the truth, a portable data folder, sha256 ids, story folders named by UTC creation time) and the
upload and sync design agreed with the owner. **Media upload is built** (package `media`, per-user `data_folder`,
resumable `PUT /upload/{sha256}`, `GET /media/{sha256}` with Range): see [docs/media.md](docs/media.md). Previews,
video posters, the change feed for stories and notes, and the story trash are not built yet.

The server will most likely run on Windows and must stay cross-platform: no cgo, Windows-safe file names (compared
ignoring case), and image or video tools (libvips, ffmpeg) run as separate programs. Check with
`GOOS=windows go build ./...`.

`.github/copilot-instructions.md` predates the KMP client (it still says "journal-client-android").
