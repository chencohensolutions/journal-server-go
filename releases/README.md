# Releases kept in the repository

`windows/journal-server-setup-<version>.exe` is the Windows installer, kept in git LFS. Download it from the file's
page on GitHub ("Download raw file"), or clone with git LFS installed (`git lfs install`).

These installers are built with `scripts/release.sh` (Inno Setup in Docker): a server installed from one doesn't
update itself. The Android app is in the app's repository, `journal-client-kmp/releases/android/`.

## Versions

The app and the server share one scheme since Nov 14, 2026: a version is the day it was made, `YYYY.MM.DD`, with
`.2`, `.3`, ... for a second or third one the same day (docs/install.md "Versions"). The server prints its version
when it starts (`journal-server 2026.11.14 (<commit> <date>)` in the log) and on `journal-server version`; the app
shows its own and the server's at the bottom of Settings.

An app and a server work together when the server's API number (`serverInfo.api`) is one the app knows. A newer
server keeps answering older apps; an older server lacks what came after it, and the app leaves those parts out
(it reads what the server can do from `serverInfo`).

| Server | API | Works with apps |
|---|---|---|
| 2026.11.14.2 | 1 | every build; apps after 2026.11.14 keep this server's own CA (`serverInfo.caCertificate`) |
| 2026.11.14 | 1 | 2026.11.14, and the builds before it |

From 2026.11.14.2 each new server makes its own CA. Apps up to 2026.11.14 reach such a server, enrolled by the QR code, only while its
server certificate keeps the key they pinned from the enrollment QR code; newer apps keep the CA too.

sha256 of journal-server-setup-2026.11.14.exe: c2864f23177260225de7fd16729483d0163547457aecfca47ab163c5b26e6e09

The app's own table is the mirror of this one, in `journal-client-kmp/releases/README.md`.
