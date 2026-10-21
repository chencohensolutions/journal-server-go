# Authentication

The journal server authenticates every device with **mutual TLS**: a device proves who it is with a
client certificate signed by the server's client CA. There are no passwords, no sessions and no
tokens on the network-facing API.

A device gets its certificate through **enrollment**: it creates a private key in its own secure
hardware, and trades a one-time enrollment code plus a certificate request for a certificate. The
private key never leaves the device.

```
Server (admin)                                   Device (Journal app)
──────────────                                   ────────────────────
journal-server enroll-code hen
  → prints address + code "K7QM-4XPD"   ─────▶   user types address + code
                                        ◀─────   query enrollmentPreview(code)          (no client cert)
  code valid for "hen", 8 min left      ─────▶   "Register this device?"
                                                 create EC P-256 key in hardware
                                                 build PKCS#10 request, signed by the key
                                        ◀─────   mutation enrollDevice(code, csr, …)    (no client cert)
  use up code, sign certificate CN=hen,
  record device in devices.json         ─────▶   store certificate next to the key
                                        ◀─────   query images                            (new client cert)
                                                 "You're connected"
```

## Access rules

| Endpoint | Without client certificate | With a valid client certificate |
|---|---|---|
| `POST /query` (GraphQL) | Only root fields marked `@public` (`enrollmentPreview`, `enrollDevice`). Everything else, including introspection, returns `client certificate required for <field>`. | Every field |
| `PUT /upload/…`, `GET /media/…` | `401` | That user's own data folder (see media.md) |
| `GET /` (playground) | Allowed (it can only call `@public` fields without a certificate) | Allowed |
| Admin API (`admin_addr`) | Loopback only, needs the admin token | n/a |

How it is enforced:

1. **TLS** (`serve.go`, `createTLSConfig`): `tls.VerifyClientCertIfGiven`. A device that is enrolling
   connects without a certificate. A certificate that *is* sent must chain to `client_ca`, or the
   handshake fails.
2. **HTTP** (`middleware.go`, `identify`): a certificate's subject CN must match a `cert_common_name` in
   `config.json`, and its serial must not be revoked in `devices.json`; otherwise `401`. The user is
   put in the request context. The media routes additionally reject requests without a certificate.
3. **GraphQL** (`auth/access.go`, `RequireUserUnlessPublic`): a root-field middleware. Anonymous
   requests may only reach fields whose schema definition carries `@public`. This is
   **deny by default**: a new query or mutation needs a certificate unless you mark it `@public`.
   Resolvers still read the user with `auth.FromContext(ctx)`.

## Enrollment codes

Created by `journal-server enroll-code <user>` (`enroll/codes.go`):

- 8 characters from `0123456789ABCDEFGHJKMNPQRSTVWXYZ` (no I, L, O, U), shown as `K7QM-4XPD`:
  40 bits, about 10¹² combinations.
- Valid for **10 minutes**, **single use**. Only the user named on the command line can be enrolled
  with it, and the user must exist in `config.json`.
- Input is normalized before lookup: case, dashes and spaces are ignored, `O` reads as `0`, and
  `I`/`L` read as `1`.
- Stored **in memory only**, as the SHA-256 of the normalized code. Restarting the server (including
  restarting the debugger) drops all pending codes, so create the code while the server is running
  and use it before the next restart.
- The command talks to the running server, so run it from the server directory (it reads
  `config.json` and `admin.token` there), e.g. `go run . enroll-code hen` next to a debug session.
- `enrollmentPreview` looks a code up without using it. `enrollDevice` uses it up atomically (lookup
  and delete under one lock), so two racing requests cannot both use it. A malformed certificate
  request is rejected *before* the code is used, so a client bug does not burn the code.
- **Guess limit:** 5 wrong codes per client IP per minute, counted across `enrollmentPreview` and
  `enrollDevice`. After that, both return `RATE_LIMITED` until the minute has passed.
- Wrong, expired and already used codes all return the same `INVALID_CODE`, so guessing reveals nothing.

## Issued certificates

Signed by `client_ca` with `client_ca_key` (`enroll/issuer.go`):

- The request must hold an **EC P-256** key (what Android Keystore and the Secure Enclave create) and
  be correctly self-signed (`INVALID_CSR` otherwise).
- Subject `CN` = the user the code was created for. The subject in the request is ignored.
- Random 128-bit serial; `ExtKeyUsage: clientAuth`; valid **365 days**, never beyond the CA's own
  expiry; `NotBefore` is backdated 5 minutes to tolerate device clock skew.
- There is no automatic renewal. When a certificate expires, enroll the device again.

## Device registry and revocation

Every enrolled certificate is appended to `devices_file` (default `devices.json`;
`enroll/devices.go`, written atomically): serial, user, device name, key protection, registration
and expiry dates, and the revocation date.

```
$ journal-server devices hen
USER   DEVICE        KEY              REGISTERED   EXPIRES      STATUS   SERIAL
hen    Pixel 9 Pro   STRONGBOX        2026-10-17   2027-10-17   active   4fa107…
hen    iPhone        SECURE_ENCLAVE   2026-09-02   2027-09-02   active   9c3e52…

$ journal-server revoke 9c3e52…
```

A user can have any number of devices. Each one enrolls with its own code (codes are single use)
and gets its own key and certificate, all with `CN = <user>`, so they share the same data. Revoking
one device does not affect the others.

A revoked serial is rejected with `401` on the next connection. Certificates that never went
through enrollment (made with `scripts/generate-user-cert.ps1`) are not in the registry and cannot
be revoked this way. To lock those out, remove the user from `config.json` or replace the CA.

`keyProtection` is what the device *reports* (`STRONGBOX`, `TEE`, `SECURE_ENCLAVE`, `SOFTWARE`). The
server does not verify it (no key attestation), so treat it as informational.

## Admin API and commands

`journal-server enroll-code | devices | revoke` are thin GraphQL clients of the running server's
admin API (`admin.go`, `admincli.go`, schema in `graph/admin/schema/admin.graphql`):

- Served on `admin_addr` (default `127.0.0.1:8081`). The server refuses to start if that is not a
  loopback address, so the admin API is never reachable from the network.
- Plain HTTP; every request needs `Authorization: Bearer <token>`. On every start the server writes a
  new random token to `admin_token_file` (default `admin.token`, mode `0600`), and the commands read
  it from there. On Windows the file mode is not enforced, so keep the server directory private to
  your account.
- The admin schema is separate from the public one, so admin operations never appear on `/query`.

## Client side (journal-client-kmp)

| | Android | iOS |
|---|---|---|
| Key store | App Android Keystore: StrongBox if present, else TEE (software on emulators) | App Keychain: Secure Enclave if available, else a software Keychain key |
| Key | EC P-256, non-exportable, `PURPOSE_SIGN` | EC P-256, `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`, `.privateKeyUsage` |
| Certificate | Stored on the Keystore entry (`setKeyEntry`) | Keychain certificate with the same label; the Keychain pairs it into a `SecIdentity` |
| mTLS | OkHttp with a key manager over the Keystore key | URLSession client-certificate challenge with the identity |

- The certificate request (PKCS#10) is built in shared Kotlin (`enrollment/CertificateRequest.kt`),
  because iOS has no API for it. Only the signature comes from the platform key.
- Enrolled identities use the alias `journal-device-<timestamp>`. A successful enrollment deletes the
  previously enrolled key. A failed or cancelled one deletes the key it created.
- Before switching to the new identity, the wizard checks it by loading the images list.
- The app opens the wizard while it has no identity, and the home screen afterwards. The home screen
  connects with the identity on every visit and shows the result (connected, rejected with `401`,
  or another failure).
- The code field accepts pasted text: it keeps the first code-shaped word (e.g. from a whole
  `Code  K7QM-4XPD` line) and drops anything that isn't a letter, digit or dash.
- Enrollment requests go out without a client certificate, but still verify the server against the
  CA bundled in the app (`sharedLogic/src/commonMain/certs/server-ca.pem`, compiled into
  `SERVER_CA_PEM`). **Keep it in sync with `certs/ca.pem`:** run
  `./gradlew :sharedLogic:syncServerCa` in the client repo and ship a new build whenever the CA changes.
- The app has no screen for certificate files any more; enrollment is the only way to get an identity.
  The `.p12` import and Android KeyChain code paths still exist in `sharedLogic` but are unused.

## Configuration

```jsonc
{
  "tls_cert": "certs/server.pem",
  "tls_key": "certs/server-key.pem",
  "client_ca": "certs/ca.pem",
  "client_ca_key": "certs/ca-key.pem",          // required: signs enrolled devices
  "public_url": "https://192.168.50.2:56398",    // optional: printed by enroll-code
  "devices_file": "devices.json",               // optional (default)
  "admin_addr": "127.0.0.1:8081",               // optional (default), must be loopback
  "admin_token_file": "admin.token",            // optional (default)
  "users": [{ "username": "hen", "cert_common_name": "hen", "data_folder": "/path/to/journal" }]
}
```

`client_ca_key` is the most sensitive file on the server: anyone holding it can mint certificates
for any user. It now has to sit next to the running server, so keep it readable only by the account
that runs the server.

## CA and server certificate

One CA (`certs/ca.pem` + `certs/ca-key.pem`) signs both the server's HTTPS certificate and the
enrolled devices' certificates. Generate them with the scripts in `scripts/` (PowerShell on Windows,
Bash on macOS/Linux):

```bash
scripts/generate-ca.sh            # .ps1 on Windows; the Bash version needs --force to replace a CA
scripts/generate-server-cert.sh   # .ps1 on Windows
```

What iOS insists on (Android is more lenient, but follow the same rules):

- **The CA must be an X.509 v3 certificate with `basicConstraints = CA:TRUE`.** A v1 certificate
  without extensions (what `openssl req -x509` produces with some `openssl.cnf` files) is not
  accepted as a trust anchor: the app's TLS handshake fails with `remote error: tls: unknown
  certificate` in the server log. The scripts now add the extensions explicitly and check the result.
- **The server certificate needs `extendedKeyUsage = serverAuth` and a `subjectAltName` listing every
  address devices use.** The scripts issue it for `localhost`, `journal.example.com` and
  `192.168.50.2` and `192.168.50.150`; edit `alt_names` in the script to add more. A device connecting by an address that
  is not listed rejects the server, e.g. `127.0.0.1` (only the name `localhost` is listed). From the
  Android emulator, run `adb reverse tcp:56398 tcp:56398` and use `localhost:56398`.

The Bash scripts write private keys with mode `0600`.

**Replacing the CA** invalidates everything it signed: the server certificate, every enrolled
device and every `.p12`. Afterwards:

1. Issue a new server certificate with the new CA and restart the server.
2. In the client repo, run `./gradlew :sharedLogic:syncServerCa` and reinstall the apps. An app built
   with the old CA rejects the server (`unknown certificate` in the server log).
3. Enroll every device again.

Renewing only the server certificate (same CA) needs none of this; just restart the server.

## Threat notes

- **Stolen enrollment code:** usable once, for at most 10 minutes, and only for the user it was made
  for. If a code may have leaked, check `journal-server devices` and revoke any device you don't recognize.
- **Network attacker during enrollment:** the device pins the server CA (`SERVER_CA_PEM`), so it
  cannot be tricked into sending the code to another server.
- **Lost or stolen phone:** `journal-server revoke <serial>`. The key cannot be extracted from the
  phone's secure hardware.
- **Anonymous probing:** without a certificate, only the two enrollment fields respond, and they are
  rate limited. The schema cannot be introspected.
