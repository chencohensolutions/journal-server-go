# Installing the server on Windows

`journal-server-setup-<version>.exe` installs the server on a Windows PC: a small installer that holds only the
program and downloads the rest while it installs. Each release on GitHub has it (Releases, below), built from
`installer/journal-server.iss` with Inno Setup.

Linux and macOS have no installer yet: each release has `journal-server-linux-amd64`, `-linux-arm64`,
`-darwin-amd64` and `-darwin-arm64`, run with launchd or systemd, and `journal-server setup` makes the folder.

## What it installs

In the folder chosen (`C:\JournalServer` by default; not Program Files, because the server writes beside itself):

| | |
|---|---|
| `journal-server.exe` | the server, its admin commands (`enroll-code`, `devices`, `copy`, …), `journal-server import` (brings photos from folders or the cloud into a user's data folder; package `importcmd`) and `journal-server jxl` (keeps a user's JPEG photos as JPEG XL, losslessly; package `jxlcmd`) |
| `onnxruntime\`, `models\` | downloaded: ONNX Runtime and every index's model (`bundle.ONNX`) |
| `vips\` | downloaded: libvips with HEIC and libjxl, from MSYS2's packages (`bundle.Vips`) |
| `ffmpeg\bin\` | downloaded: ffmpeg and ffprobe (`bundle.FFmpeg`, a pinned release of Gyan's build) |
| `config.json`, `certs\`, `devices.json` | the server's own: made by setup, or copied from the server it replaces |

The folder is added to the PATH, so `journal-server` runs from any console. It changes to its own folder when the
current one has no `config.json` (`auth.UseInstallFolder`); folders given to `journal-server import` are taken from
where it was started. Until 1.2 the import and jxl were programs of their own (`journal-import.exe`,
`journal-jxl.exe`); installing over such an install removes them. `journal-server import --cloud` needs rclone
(`winget install Rclone.Rclone`), which isn't bundled.

The server looks for libvips in its own `vips\` first, then on the PATH. A server run from a fresh git clone has
no `vips\` (it is gitignored) and falls back to whatever libvips is on the PATH, such as the official build that
`winget install libvips` puts there: the same version, but its libjxl turned down some photos as JPEG XL previews
(`jxlsave_target: JxlEncoderAddImageFrame error`) and it has no HEVC decoder for iPhone HEIC photos. So on Windows
the server logs a WARNING at start when the libvips it found is not its own `vips\` and config.json names no
`"vips"`; `go run ./scripts/bundle-vips` (or copying `vips\` from another server folder) and a restart fix it.

Every download is checked against a sha256 its source publishes (or one kept in `bundle/`), and is kept between
runs in `%LOCALAPPDATA%\journal-server`.

## The steps

1. The wizard asks: **move my existing server** (the old server's folder) or **set up a new server** (the first
   user's name and data folder, which CA, and the search model's size).
2. It copies the programs, then runs `journal-server setup` in a console, which writes the config and certificates
   and downloads (about 3 GB with every model).
3. `journal-server service install` sets up the **JournalServer** service: started automatically shortly after boot,
   before anyone logs in, and restarted if it stops on an error. It also adds the Windows Firewall rule
   "Journal server" for the port in `config.json`. Then `service start`.

The service starts in the install folder and writes its log to `server.log` there (Start menu: "Journal server
log"). `journal-server service stop|start|uninstall` (as administrator) does what it says.

## Moving a server to another PC

`journal-server setup --dir C:\JournalServer --move-from <old folder>` copies the old server's `config.json`, the
certificate files it names and `devices.json`, so enrolled phones keep working. It **only reads** the old folder:
nothing there is changed, and `admin.token` isn't copied (the server writes a new one at start).

Data folders are not copied by setup: move them yourself (to the same drive and path, or anywhere you then set as
`data_folder` in `config.json`). Setup warns about each data folder it can't find.

Phones reach the server by the address in `public_url`, and the server certificate must name it. The easiest move
gives the new PC the old one's address (a DHCP reservation on the router), with the old server stopped first.
Otherwise setup warns which of this PC's addresses the certificate lacks; run it again with `--address <new
address> --reissue-server-cert` (the old certificate is kept as `server.pem.bak`) and change `public_url`.

## A new server, and the CA

`journal-server setup --dir <folder> --user <name> --data-folder <folder> (--import-ca <folder> | --new-ca)`
writes a `config.json` with that user, makes the data folder if there is none (an existing one is used as it is:
setup writes nothing into it), and makes the server certificate for `localhost`, the PC's name, each of its private
IPv4 addresses and every `--address`. `public_url` is the first `--address`, else the first private address.

The apps trust only the CA compiled into them (`SERVER_CA_PEM` in `../journal-client-kmp`, from `syncServerCa`), so
a new server should **import that CA** (`ca.pem` and `ca-key.pem`). `--new-ca` makes another one (`package certs`,
the same kind `scripts/generate-ca.sh` makes); the published apps won't connect to such a server until they learn
each server's CA at enrollment, which is not built yet.

## Running setup again, upgrading, uninstalling

Setup keeps whatever is there (`config.json`, the CA, the server certificate, `devices.json`) and only makes or
downloads what is missing, so it is also the repair. Running the installer again over an install skips the wizard's
questions, stops the service, replaces the programs, runs setup and starts the service again.

Uninstalling removes the service, the firewall rule, the programs and what was downloaded. It never removes
`config.json`, `certs\`, `devices.json`, `server.log` or any data folder.

## Versions

The app and the server share one scheme since Nov 14, 2026: a version is the day it was made, `YYYY.MM.DD`
(`2026.11.14`), with `.2`, `.3`, ... for a second or third one the same day (`2026.11.14.2`). The date says how old a
build is and which app and server belong together; whether they work together is the server's API number
(`serverInfo.api`, `APIVersion` in `graph/resolver/resolver.go`, raised when a change needs newer apps) and the
features `serverInfo` lists, which the app checks itself. The table of which app works with which server is in
`releases/README.md`. Versions before that day were `1.x.y` (the last was 1.2.8); a date is newer than all of them.

The version is `version/VERSION`, embedded in the program by package `version`. `journal-server version` prints it
with the build's commit (`2026.11.14 (3f2a9c1 2026-11-14)`), and so do `serverInfo.version` and the installer's name.

- It changes by itself: the pre-commit hook (`.githooks/pre-commit`) runs `scripts/bump-version.sh` on every commit
  that changes Go code (not only `_test.go` files) or `go.mod`/`go.sum`, which sets today's date (or today's next
  `.N`) and adds VERSION to the commit. So the version is the day of the last change to the program.
- `scripts/bump-version.sh 2026.11.20` sets an exact one; the hook keeps a version that the commit already changes.
- `SKIP_VERSION_BUMP=1 git commit ...` leaves it as it is, for example when amending (an amend otherwise bumps again).
- The hook is on per clone: `git config core.hooksPath .githooks` (Git for Windows runs it too).

`scripts/release.sh` refuses to build while Go changes are uncommitted, so every installer's version names a commit.

## Building it

```bash
scripts/release.sh               # dist/journal-server.exe and the installer, on this machine
```

That is for trying the installer out; published releases come from GitHub (below), and only they update themselves.
It cross-compiles the program (no cgo) and compiles `installer/journal-server.iss` with Inno Setup 6 in
Docker (`amake/innosetup`). On Windows: build the programs into `dist\` and run
`ISCC.exe /DAppVersion=<version in version\VERSION> installer\journal-server.iss`.

## Releases

`.github/workflows/release.yml` publishes a release on GitHub. Start it from the repository's Actions tab
("Release", Run workflow), which releases the version in `version/VERSION` as the tag `v<version>`, or push that tag
yourself. It refuses a version that already has a release, and a tag that isn't `version/VERSION`. It runs the
tests, then builds:

- `journal-server-windows-amd64.exe`, `-linux-amd64`, `-linux-arm64`, `-darwin-amd64`, `-darwin-arm64`: the program
  for each system, marked as a release build (`version.Build` is `release`; only these update themselves),
- `journal-server-setup-<version>.exe`, the Windows installer, with Inno Setup in Docker,
- `SHA256SUMS`: `# journal-server <version>`, then the sha256 of every file,
- `SHA256SUMS.sig`: an ed25519 signature of `SHA256SUMS` by the release key.

Releases cost nothing: GitHub Actions and Releases are free for a public repository.

**The release key.** The workflow signs with the repository secret `RELEASE_SIGNING_KEY`, and every server checks the
signature against the public keys compiled into it (`update/release-keys.txt`). To make one, once:

```bash
go run ./scripts/sign-release new-key ~/journal-release.key   # prints the public key
```

Add the public key as a line of `update/release-keys.txt` and commit it; add the file's contents as the secret
(repository Settings, Secrets and variables, Actions, `RELEASE_SIGNING_KEY`), then keep the file somewhere offline.
The signature means a server installs only what this key signed: a file changed on its way, or on GitHub by anyone
without the key, is refused. Someone who could change the repository's secrets and workflow could still sign; the
stricter way, signing on the Mac before publishing, isn't built. A new key can take over by listing both in
`release-keys.txt` for a release or two. `go run ./scripts/sign-release verify dist/SHA256SUMS` checks a signature.

## Updates

A release build looks for a newer release on GitHub (package `update`, `selfupdate.go`), as `"update"` in
`config.json` says:

| `"update"` | |
|---|---|
| `"auto"` (default) | about a minute after start, writes to the log whether a newer release is out; every day at `"update_at"` (`"04:00"` by default) installs a newer release and restarts |
| `"notify"` | only writes to the log, at start and daily, that a newer release is out |
| `"off"` | doesn't look |

A build from source (`go run .`, `go build`, `scripts/release.sh`) never updates itself, whatever `"update"` says,
so a server run from a git clone stays on what was built there.

Installing a release:

1. Reads the latest release, then `SHA256SUMS` and its signature. Nothing is used unless the signature is by a key
   in `update/release-keys.txt`, and the version it names (not the tag) is newer than the one running.
2. Downloads this system's program beside the running one as `journal-server.exe.new`, checks its sha256 against
   `SHA256SUMS`, and runs `journal-server.exe.new version`, which must answer that version: a program that doesn't
   run on this PC is never put in place.
3. Renames the running program to `journal-server.exe.old` (replacing the one before) and the new one into its place.
   Windows allows renaming a running program, though not deleting it.
4. Runs the new program's `setup --dir <install folder>` when `config.json` is there, which downloads anything the new
   version needs that isn't there yet (a newer ffmpeg, a new model) and changes nothing else.
5. Stops the server the usual way (the reader finishes its file, index caches are saved) and starts the new program:
   as the JournalServer service it exits without telling the service manager, which starts it again 10 seconds
   later (the recovery actions `service install` set); from a console it starts the new program in the same window;
   on Linux and macOS it replaces itself in the same process, so systemd or launchd see no change. Uploads in
   progress go on where they stopped once the phone reconnects.

Every newer date version installs by itself. Among the old `1.x.y` versions only, a new major version (2.0 after
1.x) was only written to the log, to be installed by hand.

By hand, in the install folder (as administrator, to restart the service):

```
journal-server update             # install the latest release now and restart the service
journal-server update --check     # only say whether a newer release is out
journal-server update --rollback  # put journal-server.exe.old back; again to undo that
```

Outside the service, restart the server yourself after `update` or `--rollback`. Uninstalling removes the `.old`,
`.new` and `.rollback` programs too.
