# Journal server (Go)

GraphQL server (gqlgen) with mTLS for a private photo journal; users and devices are identified by client
certificate. Enrollment (one-time codes, certificate issuing, device registry, admin API) is built: see
[docs/authentication.md](docs/authentication.md).

The clients and the overall design live in `../journal-client-kmp`. **Read its
[docs/design.md](../journal-client-kmp/docs/design.md)** before server work: it records the agreed data format
(plain files as the truth, a portable data folder, sha256 ids, story folders named by UTC creation time) and the
next step, **upload and sync**, which the owner wants to discuss before anything is built. Nothing for media
storage or upload exists on the server yet.

`.github/copilot-instructions.md` predates the KMP client (it still says "journal-client-android").
