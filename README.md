# Go GraphQL Server

A GraphQL API server built with Go using gqlgen.

## Prerequisites

- Go 1.21 or higher
- gqlgen (install with `go get github.com/99designs/gqlgen`)

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
- `users` are mapped using `cert_common_name`
- Without a client certificate, only the enrollment fields of the GraphQL API respond
- The `/images/` route serves files only from the authenticated user's folder

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

4. Add the user to `config.json`, matching `cert_common_name` to the client cert Common Name:
   ```json
   {
     "username": "alice",
     "cert_common_name": "alice",
     "image_folder": "images/alice"
   }
   ```

4. Use the generated client cert/key on the client side to connect with mTLS.

> If you already have your own CA, call `generate-user-cert.ps1` with its `-CAKeyPath` and `-CACertPath`, then add the user entry to `config.json`.

## Project Structure

```
.
├── auth/                     # Config, request identity, @public access rule
├── docs/authentication.md    # How devices authenticate and enroll
├── enroll/                   # Enrollment codes, certificate issuing, device registry
├── graph/
│   ├── admin/                # Admin API (loopback only): schema, generated code, resolvers
│   ├── schema/
│   │   └── schema.graphql    # GraphQL schema definitions
│   ├── generated/            # Auto-generated GraphQL code
│   ├── model/                # Data models
│   └── resolver/             # Resolver implementations
├── main.go                   # Entry point and command dispatch
├── serve.go                  # HTTPS server, TLS and identity middleware
├── admin.go, admincli.go     # Admin API listener and the enroll-code/devices/revoke commands
├── gqlgen.yml               # gqlgen configuration
├── gqlgen.admin.yml         # gqlgen configuration for the admin API
└── go.mod                   # Go module definition
```

## Development

### Add a new GraphQL type:

1. Edit `graph/schema/schema.graphql`
2. Run `go tool gqlgen generate`
   - New root fields require a client certificate. Add `@public` only for fields a device must reach
     before it has one.
3. Implement resolvers in `graph/resolver/`

### Build for production:

```bash
go build -o bin/server .
```
