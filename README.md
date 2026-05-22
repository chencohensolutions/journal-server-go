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

2. **Generate GraphQL code:**
   ```bash
   go run github.com/99designs/gqlgen generate
   ```

3. **Create `config.json`:** copy `config.example.json` and fill in the certificate paths.

4. **Run the server:**
   ```bash
   go run main.go
   ```

The GraphQL playground will be available at `https://localhost:8080/`

### TLS / client certificate auth

This server uses mutual TLS for the GraphQL endpoint and image assets.

- `config.json` must include `tls_cert`, `tls_key`, and `client_ca`
- The server requires client certificates signed by the `client_ca`
- `users` are mapped using `cert_common_name`
- The `/images/` route serves files only from the authenticated user's folder

A simple `config.example.json` is included with sample user entries.

### Generating certificates

A small helper script is provided under `scripts/`.

1. Generate a local CA (once):
   ```powershell
   .\scripts\generate-ca.ps1
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

3. Add the user to `config.json`, matching `cert_common_name` to the client cert Common Name:
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
├── graph/
│   ├── schema/
│   │   └── schema.graphql    # GraphQL schema definitions
│   ├── generated/            # Auto-generated GraphQL code
│   ├── model/                # Data models
│   └── resolver/             # Resolver implementations
├── main.go                   # Application entry point
├── gqlgen.yml               # gqlgen configuration
└── go.mod                   # Go module definition
```

## Development

### Add a new GraphQL type:

1. Edit `graph/schema/schema.graphql`
2. Run `go run github.com/99designs/gqlgen generate`
3. Implement resolvers in `graph/resolver/`

### Build for production:

```bash
go build -o bin/server main.go
```
