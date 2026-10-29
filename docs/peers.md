# Copies on a friend's server (peers)

A user's copy can be kept on a friend's journal server, a **peer**, as well as in a folder on another disk
([copies.md](copies.md)). A copy at a friend's house survives a fire, a theft or a break-in that would take the
server and its USB disk together. When the friend's server also keeps a copy here, the two **swap**: each keeps the
other's copy, with no cloud account and no fees. A swap is two peerings, one each way.

Peers are optional and off by default. A copy there is plain, which the friend's admin can read, so the peer must
be marked trusted; or encrypted ("Encrypted copies" below), which nobody can read without the owner's key file.
Restore is the next part of the plan
([../../journal-client-kmp/docs/plans/swap-and-encryption.md](../../journal-client-kmp/docs/plans/swap-and-encryption.md)).

## Setting it up

On the friend's server (the **host**), set where peers' copies go, then invite this server:

```json
{ "peers_folder": "F:\\Peers" }
```

```bash
journal-server peer invite --name hen-home --quota 2TB   # the server must be running; prints the command below
```

It prints a one-time code, valid 10 minutes, followed by a dot and the host's **fingerprint** (the sha256 of its
server certificate's key). On the owner's server:

```bash
journal-server peer add dana https://dana.example:56398 K7QM-4XPD.9f86d08188… --trusted
```

and then, in config.json, a copy to the peer for each user it should keep, and a restart:

```json
{ "username": "hen", "data_folder": "D:\\Journal\\hen",
  "copies": [
    { "folder": "E:\\Journal copy\\hen", "realtime": true },
    { "peer": "dana", "at": "02:00", "bandwidth_kbps": 20000 }
  ] }
```

- `peer` names the peer as `peer add` did; a user has at most one copy per peer. `at` and `realtime` work as for
  folders.
- `bandwidth_kbps` (optional) caps how fast a copy sends, so a pass doesn't fill the home upload.
- `--trusted` says the owner accepts that the host's admin can read the copy. Without it, the server refuses a plain
  copy to the peer at start; an encrypted one (`"encrypt": true`) goes to any peer.
- The server refuses to start when a copy names a peer that isn't added.

```bash
journal-server peers                # both ways: the peers our copies are on, and those whose copies are here
journal-server peer revoke <name>   # on the host: refuse a peer from now on; its files stay in peers_folder
journal-server peer remove <name>   # on the owner's: stop using a peer (take it out of config.json first)
```

`peer remove` sets the peer's folder aside (`certs/peers/<name>.removed-<time>`) rather than deleting it. Its key
can still read the files at the host.

## Peering

- `peer add` enrolls this server with the host the way a device enrolls: it makes an EC P-256 key, sends a
  certificate request with the code to `POST /peer/enroll`, and gets a client certificate signed by the host's client
  CA. The certificate has `CN=<the invite's name>` and `OU=journal-peer` (`enroll.PeerUnit`), and lasts as long as a
  device's (365 days). The key, the certificate and `peer.json` (address, fingerprint, trusted, the name the host
  gave, quota) go in `peers_dir/<name>/` (default `certs/peers/`, gitignored with the rest of `certs/`). The server
  needn't be running.
- **Trusting the host.** The host's certificate is signed by its own authority, which this server doesn't know.
  Instead, every connection checks that the host's key has the fingerprint the invite carried. A host that makes a
  new server key needs a new invite.
- On the host, `peers.json` (beside `devices.json`, `peers_file`) lists each peer: name, serial, quota, added,
  expires, revoked. Codes are kept in memory only, like enrollment codes, and wrong codes count against the same
  per-address limit.
- **A peer certificate reaches only the peer API**, and only its own space: `identify` refuses a certificate marked
  `journal-peer` on every other route, whatever its CN. A device certificate can't reach the peer API.
- **A server set up again** (the old one lost with its `certs/`): the host's admin revokes the old peer and invites
  the new one by the same name. The new peer then has the old one's files, so a restore can read them. Only the
  host's admin can do this.

## The peer API

Over mTLS with the peer certificate. The host keeps each peer's files in `peers_folder/<peer>/<user>/`, in the data
folder's layout. The host only stores files: it makes no previews or indexes from them and never shows them to
phones. Defined in `peers/protocol.go`:

| Route | |
|---|---|
| `POST /peer/enroll` | `{code, csr}` → `{name, quota, certificate}`; anonymous, rate limited |
| `GET /peer/space` | `{quota, used, free}`: free is the quota left, or the host's free disk space less 1 GB if smaller |
| `GET /peer/{user}/list?prefix=media/2026/&recursive=1` | the files under a folder, one JSON line each (path, size, modified); dot names left out |
| `HEAD`, `GET /peer/{user}/files/{path}` | the file, with `Range`; `Journal-Modified` is its time to the nanosecond |
| `PUT /peer/{user}/files/{path}` | the file, with `Journal-Sha256` and `Journal-Modified`; whole or not at all |
| `PATCH /peer/{user}/files/{path}` | sets `Journal-Modified` |
| `DELETE /peer/{user}/files/{path}` | only for the JPEG XL rule below |
| `GET /peer/{user}/sha256/{path}` | `{sha256, size}`, hashed by the host |
| `HEAD /peer/{user}/upload/{sha256}` | `Upload-Offset`: how much of a resumed put is there |

- **Puts are checked.** The host writes to a temp file, checks the bytes against `Journal-Sha256` and only then
  renames the file into place. A file of 4 MB or more is sent with `Content-Range`: what arrives is kept in
  `<user>/.journal-partial/<sha256>`, so a put cut off by the network goes on where it stopped.
- **Quota.** A put that would take the peer past its quota answers 507 `QUOTA_FULL`, and the pass stops, as it does
  on a full disk. Errors are JSON, `{error, message}`, with the codes in `peers/protocol.go`.
- Paths are checked to stay in the peer's space (`filepath.Localize`); `.journal-partial` can't be named.

## On the owner's server

A copy to a peer is a copy like the others (`copies.Copier`): the same daily and realtime passes, `copy` /
`copy report`, `.journal-copy.json` (written to the peer), progress, `startCopy` / `stopCopy`, and the app's Copies
screen. Only the target differs: `copies.Peer` instead of `copies.Folder` (`copies/target.go`).

- **Listings, not stats.** `copies.Peer` lists each year of `media/` once and keeps the listing for 30 minutes,
  updated as it puts and removes, since only this server writes there. A pass costs a few requests besides the files
  it sends, not one per file.
- **Network errors.** A file that fails on the network is tried again after 5 s, 30 s and 2 min (resumed, for a large
  one). Then the pass stops, and the copy is out of reach (`PEER_UNREACHABLE`) until a later pass or compare reaches
  the host. A host that refuses the certificate (revoked) isn't tried again.
- **JPEG XL.** A JPEG the data folder now keeps as JPEG XL leaves a peer's copy too, as it leaves a copy in a folder
  ([copies.md](copies.md)). Downloading both to decode would cost twice their size, so the host hashes them
  instead (`GET …/sha256`). The JPEG is deleted when it has the sha256 the JPEG XL's `encodedFrom` names and the copy's
  JPEG XL is the data folder's. The decoding check already ran on this server when the JPEG XL was made.
- **In the apps:** `copies` shows a peer's copy with `kind: PEER`, the peer's name in `folder` (which names it in
  `startCopy`), `freeBytes` as what is left of the quota, and the problems `PEER_UNREACHABLE` and `QUOTA_FULL`.
  `serverInfo.peerCopies` says the server knows about them.

## Encrypted copies

A copy on a peer can be encrypted, so the friend can't read it; then the peer needn't be trusted.

```bash
journal-server copy-key new hen --out E:\hen-copy.journalkey [--passphrase]   # once per user; refuses to overwrite
```

```json
{ "peer": "dana", "at": "02:00", "encrypt": true }
```

- `copy-key new` makes an **age X25519 key pair** and a random 32-byte **naming key**. The key file (the private key,
  the naming key and the user, as age comments, so `age -d -i <key file>` opens any file) goes where the owner says,
  and the owner types back the last 6 characters of its key to show it was saved; otherwise nothing is kept.
  `--passphrase` seals the key file itself with age's passphrase mode (armored). This server keeps only the public
  key and the naming key, in `copy_keys_dir/<user>.json` (default `certs/copy-keys/`). **Without the key file the
  copy can't be read, by anyone.**
- **Encrypted here, before sending.** Each file is encrypted with age to the public key into
  `peers_dir/<peer>/copies/<user>/sending/<sha256>.age` on this server, checked against its sha256 on the way, then
  sent. It stays there until the host has it, so a send cut off resumes with the same bytes; a pass needs free space
  for the largest file it sends. Progress counts what is sent, not the encrypting.
- **Names.** A file is kept at `blobs/<2 hex>/<HMAC-SHA256(naming key, lower-case path)>`, with the time it was
  sent, so the friend sees neither names nor dates.
- **The manifest** lists each file: its path, its blob, its size and sha256, its modified time and its encrypted
  size. This server keeps it in `peers_dir/<peer>/copies/<user>/manifest.json` (with `.journal-copy.json`), and works
  out what the copy lacks from it, with no request. It is gzipped and sealed (XChaCha20-Poly1305, a key from the
  naming key) to the peer after each full pass, and at most hourly after realtime batches, as `manifest-a.sealed`
  and `manifest-b.sealed` in turn; the newer is read. When this server's own is lost, it is read back from the peer.
- **Checked against the peer.** A compare and a full pass start by listing the peer's blobs: a file whose blob is
  gone, or isn't the size sent, leaves the manifest and is sent again.
- A changed sidecar is sent again under the same name. A JPEG kept as JPEG XL leaves the copy when the manifest's
  sha256s match, as for plain peers.
- **What the friend sees**: how many files, their sizes (age adds a little; sizes aren't hidden) and when they
  were sent. Not content, names, dates or places.
- In the apps, `DataCopy.encrypted` is true.

## Code

- `peers/`: the host side. `registry.go` (`peers.json`), `host.go` (invites, enrolling, the API and `Mount`),
  `protocol.go` (routes, headers, codes, `Fingerprint`).
- `copies/peer.go`: `copies.Peer`, the target, and `PeerClient` (the pinned TLS client).
- `copies/encrypted.go`: `copies.Encrypted`, the encrypted target over a `Peer`; `CopyKey`, blob names and the
  sealed manifest. `copykey.go`: `copy-key new`.
- `peer.go`: `peer add | invite | revoke | remove`, `peers`, loading a peer for config.json's copies, and
  `openPeerHost`.
- `enroll.Issuer.IssuePeer` and `enroll.IsPeer`; `identify` in `middleware.go` refuses peer certificates.
- The admin schema's `peers`, `createPeerInvite` and `revokePeer`.

## Not built yet

Restore, peers' certificates renewing, and serving phones from a peer while the owner's server is
off. See the plan.
