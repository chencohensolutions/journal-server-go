# Journal server

The server of a private photo journal: a GraphQL API (gqlgen) over mutual TLS, with resumable uploads of
photos and videos into a plain folder of files. Devices are identified by client certificates they get by
enrolling. See [docs/authentication.md](docs/authentication.md) and [docs/media.md](docs/media.md); the clients
and the overall design are in `../journal-client-kmp`.

## Prerequisites

- Go 1.26 or higher (gqlgen is a `go tool` of the module; nothing to install)
- libvips (`brew install vips`) for previews and ffmpeg (`brew install ffmpeg`) for video posters. Without
  them the server runs and those endpoints answer 503.

## Setup

1. **Install dependencies:**
   ```bash
   go mod download
   ```

2. **Generate GraphQL code** (public API and admin API):
   ```bash
   go tool gqlgen generate
   go tool gqlgen generate --config gqlgen.admin.yml
   ```

3. **Create `config.json`:** copy `config.example.json` and fill in the certificate paths.

4. **Run the server:**
   ```bash
   go run .
   ```

The GraphQL playground will be available at `https://localhost:56398/`

### Authentication

Devices authenticate with client certificates (mutual TLS). The usual way to give a device one is
**enrollment**: run the server, then

```bash
go run . enroll-code alice     # one-time code, valid 10 minutes
```

and type the server address and code into the Journal app ("Register this device"). The app creates
a key in the phone's secure hardware and the server signs a certificate for it.

```bash
go run . devices [user]        # list enrolled devices
go run . revoke <serial>       # lock a device out
```

- `config.json` must include `tls_cert`, `tls_key`, `client_ca` and `client_ca_key` (signs enrolled devices)
- A client certificate's Common Name is the `username` it belongs to
- Without a client certificate, only the enrollment fields of the GraphQL API respond
- The media routes (`/upload/`, `/media/`) serve only the authenticated user's data folder

See [docs/authentication.md](docs/authentication.md) for the full design. A `config.example.json` is
included with a sample user entry.

### Generating certificates

A small helper script is provided under `scripts/`. With enrollment, you only need the CA and the
server certificate. User certificates from step 2 are still accepted, e.g. for clients that
cannot enroll.

1. Generate a local CA (once):
   ```powershell
   .\scripts\generate-ca.ps1
   ```
   or on macOS/Linux:
   ```bash
   scripts/generate-ca.sh        # --force to replace an existing CA
   ```

2. Generate a new user certificate:
   ```powershell
   .\scripts\generate-user-cert.ps1 \
     -CAKeyPath certs/ca-key.pem \
     -CACertPath certs/ca.pem \
     -CommonName alice
   ```

   If OpenSSL on `PATH` is broken, pass the working binary and config explicitly:
   ```powershell
   .\scripts\generate-user-cert.ps1 \
     -CAKeyPath certs/ca-key.pem \
     -CACertPath certs/ca.pem \
     -CommonName alice \
     -OpenSSLPath 'C:\Program Files\OpenSSL-Win64\bin\openssl.exe' \
     -OpenSSLConfig 'C:\Program Files\Common Files\SSL\openssl.cnf'
   ```

3. Generate the server TLS key/cert for localhost:
   ```powershell
   .\scripts\generate-server-cert.ps1 \
     -OpenSSLPath 'C:\Program Files\OpenSSL-Win64\bin\openssl.exe' \
     -OpenSSLConfig 'C:\Program Files\Common Files\SSL\openssl.cnf'
   ```

4. Add the user to `config.json`; the `username` must be the client cert's Common Name:
   ```json
   {
     "username": "alice",
     "data_folder": "journal/alice"
   }
   ```

4. Use the generated client cert/key on the client side to connect with mTLS.

> If you already have your own CA, call `generate-user-cert.ps1` with its `-CAKeyPath` and `-CACertPath`, then add the user entry to `config.json`.

## Project Structure

```
.
├── main.go                   # Entry point: serve, or run an admin command
├── serve.go                  # HTTPS server, TLS config, routing table (newMux)
├── middleware.go             # identify (certificate → user), gzip, request deadlines
├── library.go                # Opens every user's data folder; finds libvips and ffmpeg
├── admin.go, admincli.go     # Admin API listener and the enroll-code/devices/revoke commands
├── auth/                     # config.json (AppConfig, LoadConfig), request identity, @public rule
├── enroll/                   # Enrollment codes, certificate issuing, device registry, rate limit
├── media/                    # Data folder: store and index, uploads, previews, posters, DNG developing,
│                             #   change feed, multi-process turns (see docs/media.md)
├── graph/
│   ├── schema/*.graphql      # Public GraphQL schema (enrollment, media, change feed)
│   ├── generated/, model/    # gqlgen output (don't edit)
│   ├── resolver/             # Resolvers
│   └── admin/                # Admin API: its own schema, generated code and resolvers
├── scripts/
│   ├── import/               # go run ./scripts/import: photos already on disk or in a cloud → data folder
│   ├── scan-nudity/          # go run ./scripts/scan-nudity: NudeNet scores into sidecars
│   ├── bundle-vips/          # go run ./scripts/bundle-vips: portable libvips for Windows
│   └── generate-*.sh/.ps1    # Local CA and certificates
├── docs/                     # authentication.md, media.md
├── gqlgen.yml                # gqlgen configuration
└── gqlgen.admin.yml          # gqlgen configuration for the admin API
```

## Development

### Add a new GraphQL type:

1. Edit `graph/schema/schema.graphql`
2. Run `go tool gqlgen generate`
   - New root fields require a client certificate. Add `@public` only for fields a device must reach
     before it has one.
3. Implement resolvers in `graph/resolver/`

### Test

```bash
go test ./...                 # preview and poster tests skip without libvips/ffmpeg
```

### Build for production:

```bash
go build -o bin/server .
```
