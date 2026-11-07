# Copies on a friend's server (peers)

A user's copy can be kept on a friend's journal server, a **peer**, as well as in a folder on another disk
([copies.md](copies.md)). A copy at a friend's house survives a fire, a theft or a break-in that would take the
server and its USB disk together. When the friend's server also keeps a copy here, the two **swap**: each keeps the
other's copy, with no cloud account and no fees. A swap is two peerings, one each way.

Peers are optional and off by default. A copy there is plain, which the friend's admin can read, so the peer must
be marked trusted; or encrypted ("Encrypted copies" below), which nobody can read without the owner's key file.
Restore is below; encrypted accounts are later parts of the plan
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
server certificate's key), and the host's address: `public_url` when set, else its public IP now (asked of a STUN
server), so a host with no host name can be invited too. On the owner's server:

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

- `peer add` takes the host's fingerprint or its **server id** (`journal-server id`: the same sha256 written as
  Syncthing writes device ids, base32 in 8 groups of 7 with check letters, so a typo is found).
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
- **Renewal.** At start and daily, this server renews each peer certificate that expires within 30 days
  (`renewPeers` in `peer.go`): a new key, `POST /peer/renew` with the current certificate, and the new pair in
  place of the old in `peers_dir/<name>/` (the old kept as `*.pem.prev`). The copies switch to it at once. The host
  accepts the old certificate (`previous_serial` in `peers.json`) until the new one is first used.
- **A peer certificate reaches only the peer API**, and only its own space: `identify` refuses a certificate marked
  `journal-peer` on every other route, whatever its CN. A device certificate can't reach the peer API.
- **A server set up again** (the old one lost with its `certs/`): the host's admin revokes the old peer and invites
  the new one by the same name. The new peer then has the old one's files, so a restore can read them. Only the
  host's admin can do this.

## Finding the host (discovery)

Since Nov 7, 2026, a peer whose home IP changes, or that has no host name, is followed by its id, as Syncthing finds
devices: the address in `peer add` is only the first guess. Every connection still checks the host's key against
the fingerprint, so an address can't make another server pass for the host; a wrong one just fails. Nothing is
relayed: the host must be reachable at one of its addresses (its port mapped on the router, by itself or by hand, or
IPv6).

- **Where a server is.** Each server works out its addresses every 5 minutes (`discovery.Own`): `public_url`, the
  router's outside address and port when the router forwards one to it (below), its public IPv4 address at `port` (from a STUN server, `discovery.STUNServers`, or as a peer saw it), its global IPv6
  addresses, then its LAN addresses.
- **Peers tell each other.** `GET /peer/addresses` (with the peer certificate) answers `peers.Discovery`: the host's
  addresses, its DHT key, and the IP the request came from (so a server learns its own public IP). `POST
  /peer/enroll` answers the same in `discovery`. The owner's server asks each peer at start and every 30 minutes
  (`askPeerAddresses` in `discover.go`).
- **The DHT.** Each server publishes its addresses in the BitTorrent DHT (BEP 44 mutable items) at start, every 45
  minutes and within 5 minutes of a change (`discovery.Publisher`). The record is kept under an ed25519 key made
  from the server's TLS key (HKDF of it, so no new file; it changes only with the key and the id), signed with it,
  and sealed (XChaCha20-Poly1305, a key made from the fingerprint) so the DHT's nodes and crawlers can't read the
  addresses: only who knows the server can. At most 1000 bytes; the last addresses are left out if more don't fit.
  The server joins the DHT passively: it answers no other node, and sends a few UDP packets per publish or lookup.
- **Dialing.** `copies.Peer` dials by the peer's address book (`copies.AddressBook`, `peerBook` in `discover.go`):
  every known address, the one that last worked first, the next started 400 ms after the one before, and the first
  connection made is kept. When none answers, the peer is looked up in the DHT (at most every 2 minutes) and the
  addresses found are dialed. The address that worked goes first in `peer.json`'s `addresses` (beside `url`, which
  stays as typed), with `discovery_key`, the host's DHT key.
- `"discovery": false` in config.json publishes nothing and looks nothing up; peers still tell each other their
  addresses.
- `journal-server id` prints this server's id, fingerprint and DHT key. `journal-server discovery` checks it from
  this network: it lists the routers that map ports and their outside addresses, prints the addresses it found,
  publishes them, looks them up again, and looks each peer up. It only asks the routers: mapping from there would
  take over the running server's mapping.

- **Phones** read the same through `serverInfo.discovery` (public schema, `graph/schema/discovery.graphql`): the
  fingerprint and id to pin, the DHT key, and the addresses (the apps' side:
  [discovery.md](../../journal-client-kmp/docs/plans/discovery.md)).

What it doesn't do: relay through a third machine (behind carrier-grade NAT the host is found but can't be reached).

### Opening the router

Since Nov 7, 2026, the server asks the home router to forward its `port` to this machine (package `portmap`), so
nobody has to forward it by hand. At start it looks for routers with UPnP IGD (SSDP; WANIPConnection 2 and 1,
WANPPPConnection) and NAT-PMP on the default gateway at once, and maps TCP `port` on the first that agrees (when
another machine holds that outside port, the next four are asked for, and the one granted is published), for an
hour (a router that takes only permanent mappings gets one). It renews at half the lease, looks for a router again
every 10 minutes while it has none (UPnP turned on, a router restarted), and removes the mapping when the server
stops, before the process ends.

- The router's outside address and the port it granted go first after `public_url` in the addresses published in
  the DHT and told to peers and phones.
- **Carrier-grade NAT.** When the router's outside address isn't public (100.64.0.0/10 or a private range: the
  provider shares one public IP between many homes, or another router sits in front), the mapping can't help: the
  log says so once, with what to do (ask the provider for a public IPv4 address, or use IPv6), and that address
  isn't published.
- Routers with UPnP turned off and no NAT-PMP: the log says once to forward the port by hand, or to use IPv6.
- `"port_mapping": false` in config.json turns it off.

## The peer API

Over mTLS with the peer certificate. The host keeps each peer's files in `peers_folder/<peer>/<user>/`, in the data
folder's layout. The host only stores files: it makes no previews or indexes from them and never shows them to
phones. Defined in `peers/protocol.go`:

| Route | |
|---|---|
| `POST /peer/enroll` | `{code, csr}` → `{name, quota, certificate, discovery}`; anonymous, rate limited |
| `GET /peer/addresses` | `{addresses, key, seen}`: where the host is, its DHT key, the caller's IP |
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
journal-server copy-key new hen --from-passphrase                            # or: no key file, a passphrase
journal-server copy-key export hen --out E:\hen-copy.journalkey             # a key file of a passphrase's key
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

- **A key from a passphrase** (`--from-passphrase`, Oct 29, 2026): the age key and the naming key are derived from
  the passphrase with Argon2id (256 MiB, 3 passes, 4 lanes) and a random 16-byte salt. The salt and the parameters
  aren't secret: this server keeps them in its `<user>.json` and each encrypted copy carries them
  (`copy-key.json` at its root, sent with the first full pass), so after losing everything the passphrase alone
  restores. Whoever holds the copy can try passphrases against it, so a passphrase needs 24 characters, or 4 words
  or more and 20 characters; six random words are best. A forgotten passphrase is a lost copy. `copy-key export`
  checks the passphrase gives the stored key and writes a key file of it, for the age tool or as a spare.

## Restore

```bash
journal-server restore hen --from dana --to D:\Journal\hen-restored [--key E:\hen-copy.journalkey]
journal-server restore hen --from-folder F:\hen-home\hen --to D:\Journal\hen-restored [--key …]
```

- `--from` reads the copy from the peer over the peer API; `--from-folder` from a folder that holds one: the
  friend's `peers_folder/<peer>/<user>/` handed over on a disk, or a plain copy on another disk ([copies.md](copies.md)).
- `--to` is always given and must be **empty**: a restore never fills a folder it wasn't pointed at, the user's
  `data_folder` included. It marks the folder (`.journal-restore.json`) while it works, so the same command run
  again goes on where it stopped; files already there with the right size and sha256 are skipped.
- A plain copy comes back file by file (`media/`, `relabels/`, the root files). An encrypted copy (it has a sealed
  manifest) needs `--key`: the key file's naming key opens the manifest, and its key decrypts each file. A key file
  sealed with a passphrase asks for it. `--passphrase` instead gives the key from a passphrase.
- `--as-of 2026-10-20` (or `"2026-10-20 15:00"`) brings back each sidecar and root file as it was at the end of that
  day, local time: the version the copy kept in `history/` when it replaced it soonest after then
  ([copies.md](copies.md), "Previous versions"). Files added since come back as they are; originals never change.
- Each original is checked against its sha256 (the manifest's, or its sidecar's for a plain copy) before it takes its
  place. A file that fails is tried 3 times, then listed at the end; the command fails and can be run again.
- Four files come back at once. The server needn't be running. Afterwards, point the user's `data_folder` at the
  folder and start the server: the indexes and previews are made again, as when `.cache/` is missing.
- **When the whole PC is lost**: install journal-server, and either restore `certs/` from a backup (the old peer
  certificate still works) or ask the friend to `peer revoke` the old peer and invite the new server **under the same
  name**, then `peer add` it. Then `restore` with the key file. Only the friend's admin can give a new server the old
  one's files.
- Copies keep the versions they replace, so a sidecar damaged on the server comes back good with `--as-of` a day
  before. Originals are never overwritten in a copy.

## Code

- `peers/`: the host side. `registry.go` (`peers.json`), `host.go` (invites, enrolling, the API and `Mount`),
  `protocol.go` (routes, headers, codes, `Fingerprint`).
- `copies/peer.go`: `copies.Peer`, the target, and `PeerClient` (the pinned TLS client).
- `copies/encrypted.go`: `copies.Encrypted`, the encrypted target over a `Peer`; `CopyKey`, blob names and the
  sealed manifest. `copykey.go`: `copy-key new`. `copies/restore.go`: `Restore` and `ReadKeyFile`;
  `restore.go`: the command.
- `peer.go`: `peer add | invite | revoke | remove`, `peers`, loading a peer for config.json's copies, and
  `openPeerHost`.
- `discovery/`: server ids (`id.go`), the DHT record and keys (`record.go`), the DHT node (`node.go`, on
  github.com/anacrolix/dht), STUN (`stun.go`), this server's addresses (`own.go`) and the publisher.
  `discover.go`: the peers' address books, asking peers, `id` and `discovery`. `copies/addresses.go`: the dialer.
- `enroll.Issuer.IssuePeer` and `enroll.IsPeer`; `identify` in `middleware.go` refuses peer certificates.
- The admin schema's `peers`, `createPeerInvite` and `revokePeer`.

## Not built yet

Pruning old versions in history/, and serving phones from a peer while the owner's server is
off. See the plan.
