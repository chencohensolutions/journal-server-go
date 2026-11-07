# Installing the server on Windows

`journal-server-setup-<version>.exe` installs the server on a Windows PC: a 10 MB installer that holds only the
three programs and downloads the rest while it installs. Built by `scripts/release.sh` (Inno Setup in Docker, from
`installer/journal-server.iss`).

## What it installs

In the folder chosen (`C:\JournalServer` by default; not Program Files, because the server writes beside itself):

| | |
|---|---|
| `journal-server.exe` | the server, and its admin commands (`enroll-code`, `devices`, `copy`, …) |
| `journal-import.exe` | `scripts/import`: brings photos from folders or the cloud into a user's data folder |
| `journal-jxl.exe` | `scripts/jxl`: keeps a user's JPEG photos as JPEG XL, losslessly |
| `onnxruntime\`, `models\` | downloaded: ONNX Runtime and every index's model (`bundle.ONNX`) |
| `vips\` | downloaded: libvips with HEIC and libjxl, from MSYS2's packages (`bundle.Vips`) |
| `ffmpeg\bin\` | downloaded: ffmpeg and ffprobe (`bundle.FFmpeg`, a pinned release of Gyan's build) |
| `config.json`, `certs\`, `devices.json` | the server's own: made by setup, or copied from the server it replaces |

The folder is added to the PATH, so `journal-server`, `journal-import` and `journal-jxl` run from any console. Each
changes to its own folder when the current one has no `config.json` (`auth.UseInstallFolder`); folders given to
`journal-import` are taken from where it was started. `journal-import --cloud` needs rclone
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

The version is `version/VERSION` (major.minor.patch), embedded in all three programs by package `version`.
`journal-server version`, `journal-import --version` and `journal-jxl --version` print it with the build's commit
(`1.4.2 (3f2a9c1 2026-10-23)`), and so do `serverInfo.version` and the installer's name.

- **Patch** goes up by itself: the pre-commit hook (`.githooks/pre-commit`) runs `scripts/bump-version.sh patch` on
  every commit that changes Go code (not only `_test.go` files) or `go.mod`/`go.sum`, and adds VERSION to the commit.
- **Minor** (new features) and **major** (changes every device or install must follow): run
  `scripts/bump-version.sh minor` (or `major`, or an exact `1.5.0`) before committing; the hook keeps a version
  that the commit already changes.
- `SKIP_VERSION_BUMP=1 git commit ...` leaves it as it is, for example when amending (an amend otherwise bumps again).
- The hook is on per clone: `git config core.hooksPath .githooks` (Git for Windows runs it too).

`scripts/release.sh` refuses to build while Go changes are uncommitted, so every installer's version names a commit.

## Building it

```bash
scripts/release.sh               # dist/journal-server.exe, journal-import.exe, journal-jxl.exe and the installer
```

It cross-compiles the three programs (no cgo) and compiles `installer/journal-server.iss` with Inno Setup 6 in
Docker (`amake/innosetup`). On Windows: build the programs into `dist\` and run
`ISCC.exe /DAppVersion=<version in version\VERSION> installer\journal-server.iss`.
