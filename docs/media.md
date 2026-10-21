# Media: storage and upload

Devices upload originals to the server, which keeps them byte for byte in the user's **data folder**
as plain files. The files are the truth: the index in memory is rebuilt from the sidecars every
time the server starts, so the folder alone is a complete copy, on Windows, macOS or Linux.

Built so far: upload (resumable, deduplicated by sha256), downloading or streaming originals, verifying an
original before a device deletes its copy, and photo previews at the exact size the client asks for, video posters, and the change feed for media. **Not built
yet:** stories and notes in the change feed, and the story trash. The server runs on **macOS** for now
(previews need libvips: `brew install vips`); Windows comes later. To run it on Windows now, run
`go run ./scripts/bundle-vips` once in the server folder (from PowerShell or cmd: Git Bash's `tar` can't read its
Windows paths): the official Windows builds of libvips (WinGet, libvips.org) leave out the HEVC decoder and can't
read iPhone HEIC photos, so this copies MSYS2's build, with JPEG XL for Samsung's DNGs,
with only the DLLs it loads (about 130 MB), into `vips/`. The server uses `vips/bin` before the PATH, and the
folder can be moved along with the server. The agreed design is in
[journal-client-kmp/docs/design.md](../../journal-client-kmp/docs/design.md) ("Upload and sync").

## The data folder

Set per user in `config.json` as `data_folder`. Users without one can't upload.

```
<data_folder>/
  media/2026/2026-10/IMG_1234.HEIC          ← the original, never modified
  media/2026/2026-10/IMG_1234.HEIC.json     ← its sidecar
  media/2026/2026-10/IMG_1234 (2).HEIC      ← a different file with the same name
  media/undated/…                           ← files the device sent no date for
  .cache/uploads/<sha256>.partial           ← an upload in progress
  .cache/uploads/<sha256>.json              ← what the device declared for it
  .cache/previews/<sha[:2]>/<sha256>/1080x1440-cover.webp   ← previews made so far
```

- The month folder is from `takenAt` in the photo's own local time (the offset the device sent).
- File names are made safe for every platform (`media/names.go`): no folders, no `<>:"/\|?*`, no
  Windows device names like `CON`, no trailing dots or spaces. A name already in the folder,
  **ignoring case**, gets ` (2)`, ` (3)`, …
- Each original is listed once. A second sidecar with the same sha256, or a sidecar whose original is
  gone, is logged and skipped (left on disk to look at).

### Sidecar (format 1)

```json
{
  "format": 1,
  "sha256": "9f2c…e1",
  "facts": {
    "fileName": "IMG_1234.HEIC",
    "kind": "photo",
    "mimeType": "image/heic",
    "size": 2345678,
    "takenAt": "2026-10-17T15:52:22+03:00",
    "width": 4032, "height": 3024,
    "durationMs": null,
    "location": { "lat": 32.0853, "lon": 34.7818 },
    "source": "camera",
    "versionOf": null,
    "pairedWith": null,
    "uploadedAt": "2026-10-19T08:01:13Z",
    "uploadedBy": "Galaxy S25 (SM-S931B)"
  }
}
```

The device sends the facts (it already read them from MediaStore/PhotoKit); the server adds
`uploadedAt` and `uploadedBy` (the enrolled device's name). `versionOf` is set on an edit of a photo
(same device id, new content). `pairedWith` is set on a Live Photo's video (it names the photo) and on
a RAW (it names the JPEG). A `note` (caption and tags) will be added to the sidecar by sync. `detected` holds what
models found in the file (see [Nudity scan](#nudity-scan)). Files brought in
by the import script have `"uploadedBy": "import"` and `importedFrom`, the path they were found at. `correctedAt`
is set when the server later corrects a fact clients show (see [Fixing sources](#fixing-sources)).

## Uploading a file

```
device                                                      server
──────                                                      ──────
sha256 of the file (on Android, of the unredacted original)
query missingMedia(hashes: [...])            ─────▶   which of these it doesn't have (≤ 1000 per call)
mutation startUpload(input: {sha256, size, …}) ───▶   UploadReady { url, offset, chunkSize }
                                                      | MediaExists (nothing to send)
                                                      | UploadRejected { reason }
PUT /upload/{sha256}                                  
    Content-Range: bytes 0-16777215/1987654321 ───▶   200 Upload-Offset: 16777216
PUT … bytes 16777216-33554431/1987654321      ───▶   200 Upload-Offset: 33554432
…                                                     
PUT … the last piece                          ───▶   checks the sha256, moves the file to
                                                      media/YYYY/YYYY-MM/, writes the sidecar
                                              ◀───   201 Upload-Offset: 1987654321
```

`PUT /upload/{sha256}` answers:

| Status | Meaning | What the device does |
|---|---|---|
| `200`, `Upload-Offset: n` | Piece stored, more expected | Send the piece starting at `n` |
| `201`, `Upload-Offset: size` | The file is stored. Also returned when a piece is repeated after that (a lost response). | Done |
| `400` | Bad `Content-Range`, `Content-Length` doesn't match it, or the piece ended early | Fix it, or resend the piece |
| `404` | No upload started for this sha256, or it expired | `startUpload` again |
| `409` | The bytes don't have the declared sha256; the upload is deleted | Hash the file again and start over (it may have been edited meanwhile) |
| `413` | Piece larger than 64 MB | Send smaller pieces |
| `416`, `Upload-Offset: n` | The piece doesn't start where the received bytes end | Continue from `n` |

- **Resuming:** `HEAD /upload/{sha256}` returns `Upload-Offset` (and `Upload-Complete`). `startUpload`
  again also returns the offset. Either works after a dropped connection or a server restart.
- **Pieces:** the server asks for 16 MB (`chunkSize`) and accepts up to 64 MB. A piece that breaks off
  is dropped whole, so the offset is always at the end of a whole piece.
- **Two devices, one file:** each sha256 is written by one request at a time, and the second device's
  `startUpload` reports `MediaExists` once the first has finished.
- **Expiry:** an unfinished upload is deleted 7 days after its last `startUpload` (checked hourly).
- **Rejections:** `UNKNOWN_ORIGINAL` / `UNKNOWN_PAIR` (upload that file first), `TOO_LARGE` (over
  32 GB), `NO_SPACE` (the upload would leave less than 1 GB free on the server's disk).

## Downloading and streaming

`GET /media/{sha256}` returns the original with its MIME type, `ETag: "<sha256>"`, and
`Cache-Control: private, max-age=31536000, immutable`, since the content of a sha256 never changes.
It supports `Range`, which is how videos stream and seek; videos are never converted.

## Verifying before a device deletes its copy

`POST /media/{sha256}/verify` reads the original from disk again and hashes it, every time (nothing is remembered).
It answers `200 {"sha256": "…", "result": "ok" | "missing" | "mismatch"}`:

- `ok`: the original and its sidecar are there and the bytes have the sha256. Only then may a device delete its copy
  (the app's "Free up space" hashes its own copy at the same time; see the client's design.md).
- `missing`: not in the index, or the original or its sidecar is gone from the folder (logged).
- `mismatch`: the original is there but its size or bytes are wrong. The server logs `THE ORIGINAL IS DAMAGED`;
  it doesn't repair it, and an upload of the same sha256 still answers `MediaExists`, so fix it by hand from the
  device's copy for now.

At most 2 files are hashed at once, so checks don't starve uploads of the disk. There is no time limit (a 32 GB
video on a slow disk takes minutes); hashing stops when the device drops the request.

## Previews

`GET /media/{sha256}/preview?w=1080&h=1440&fit=cover&crop=0.1,0.2,0.5,0.5&fmt=webp&q=high`

| Parameter | Meaning |
|---|---|
| `w`, `h` | The **physical pixels** the client will draw, 1–4096. The answer has exactly this size, so it is drawn 1:1 with no scaling on the device. |
| `fit` | `cover` (default): fills the box, centered, cutting off what doesn't fit. `contain`: fits inside the box, so one side may be shorter. |
| `crop` | Optional `x,y,w,h` as fractions of the **upright** photo (rounded to 1/10000): a story tile's focus and zoom. It is cut from the full-resolution original, so a 4× zoom stays sharp. |
| `fmt` | `webp` (default), `jpeg`, `heif` (HEVC, 4:2:0 so phones decode it), `avif` (AV1, 4:2:0) or `jxl` (JPEG XL). |
| `q` | `low`, `medium`, `high` (default) or `max`: libvips Q 50, 70, 85, 95. |

- Made with libvips in one scaling step from the original (`vips thumbnail`, then `extract_area` when
  something is cut off). EXIF orientation is applied, and the colour profile (Display P3) is kept;
  other metadata is dropped. Quality 90.
- **DNG (RAW)** previews show the raw data, never the JPEG a DNG carries. Most DNGs are read by libvips
  (LibRaw). Samsung's Expert RAW and Pro mode DNGs (DNG 1.7: demosaiced "linear" data compressed with JPEG XL,
  which LibRaw can't unpack) are developed by the server (`media/develop.go`): libvips decodes the JPEG XL, then
  the DNG's own colour processing is applied (black and white levels, white balance and its ColorMatrix /
  ForwardMatrix to sRGB, BaselineExposure, the ProfileGainTableMap that lifts the shadows, a soft roll-off of
  highlights, its default crop and orientation). The result is kept full size as `developed.jpg` beside the
  photo's previews when the user's `preview_cache` is on (about 2.5 s for a 24 MP photo, so with the cache off
  every preview of such a DNG takes that long).
  libvips needs JPEG XL support for this: Homebrew's has it, and so does the Windows bundle.
- **Never scaled up.** When the box is bigger than the photo, the preview keeps the box's shape at the
  photo's own resolution (so it's smaller than asked); the client scales it on screen.
- Served with `Cache-Control: immutable`; devices keep each size they ask for, and ask for a size only when they
  have neither it nor the original. **Kept on the server only with `"preview_cache": true`** in the user's entry in
  `config.json` (off by default): then under `.cache/previews/`, and when they pass `preview_cache_max_gb` (default
  50) the least recently used are deleted (checked hourly). Without it each preview is made in a temporary folder,
  sent and deleted.
- **Videos** get their poster: one frame about 1 s in (halfway through a shorter one), taken by **ffmpeg** with the
  video's rotation applied, HDR (HLG or PQ) tone-mapped to standard range, then made into the preview like a photo.
  ffmpeg is found like libvips (`"ffmpeg"` in config.json, the PATH, or Homebrew; on Windows `winget install
  Gyan.FFmpeg`). The frame is kept as `poster.jpg` only with `preview_cache` (about 2 s for a large video on a disk).
- Answers: `422` bad parameters, `404` unknown sha256, `503` when libvips (or, for a video, ffmpeg) isn't installed (the server logs this at startup; set `"vips"` in config.json if it's
  installed somewhere unusual).

`query media(sha256)` returns the facts, plus `versions` (edits of this photo) and `paired` (files
that name it in `pairedWith`).

`query storageUsage` returns how many photos and videos the user has and the bytes their originals take
(`photoBytes`, `videoBytes`, `bytes`), counted from the index, so it answers at once. Sidecars, uploads in
progress and previews aren't counted. The app shows it in Settings → Backup.

All of these need a client certificate, like everything except enrollment
(see [authentication.md](authentication.md)).

## Change feed

`query changes(since: String, limit: Int = 2000)` returns pages of `Change { kind: MEDIA, path: "media/<sha256>",
deleted, media }`, oldest change first, with a `cursor` and `hasMore`. A new device starts without `since` and calls
again with each page's cursor until `hasMore` is false; afterwards it asks with its saved cursor for what changed
since (an empty page returns the same cursor). A file changes when it is uploaded, and again when a scan writes its
answer into the sidecar (see [Nudity scan](#nudity-scan)) or its source is corrected
(see [Fixing sources](#fixing-sources)): it then comes again, whole. The cursor is opaque (today
the time of the last change and sha256 of the last original in the page), so stories and notes can join the feed later
without changing clients. Pages are at most 5000; responses to `/query` are gzipped when the client accepts it.

## Nudity scan

`go run ./scripts/scan-nudity --user hen` scores every photo and video for nudity with
[NudeNet](https://github.com/notAI-tech/NudeNet) (`python3 -m pip install nudenet`; `--python` picks another Python)
and writes the answer into the sidecar:

```json
"detected": {
  "nudity": { "score": 0.93, "model": "nudenet-320n", "scannedAt": "2026-10-20T19:24:10Z" }
}
```

- The score is NudeNet's highest confidence in an exposed breast, genitals, buttocks or anus; 0 when it finds none.
- It is never a tag: the user's tags stay theirs. The API gives it as `Media.nudity` (null until scanned). Clients
  hide what is over the user's threshold while "Show private media" is off, and fetch nothing of it (no preview, no
  original).
- NudeNet runs in Python as a separate program and is given a 640 px JPEG preview (a video's poster frame), never the
  original. Files already scanned by the same `model` are skipped, so a stopped run continues where it stopped;
  `--rescan` scans everything again. The server may keep running (see "Several processes on one data folder").
- For now it is run by hand after uploads or imports; later it will run on each new file.

## Fixing sources

`go run ./scripts/fix-sources --user hen [--dry-run]` corrects, once, the `source` of files WhatsApp named
(`IMG-20230115-WA0012.jpg`, `VID-…-WA…`; also `AUD`, `PTT`, `STK`, `DOC`) that came in as `camera` or `other`, so
the apps hide them with the rest of WhatsApp. Imports used to tell the source from the folder only, so a WhatsApp
file copied elsewhere before it was imported lost it; they now look at the name too.

- Only `facts.source` and `facts.correctedAt` change in the sidecar (inside a turn); originals are never touched.
  `correctedAt` makes the file come again in the change feed, so phones that already have it learn the new source.
- It prints how many it corrected, their sources before, and where they were found (the first folders of
  `importedFrom`), to tell why they had the wrong source. Files whose folders mention WhatsApp but whose name
  doesn't are listed and left as they are. Running it again changes nothing.

## Importing files already on the server's disk

`scripts/import` copies photos and videos from folders on the server's disk (old phone backups) into a user's
data folder, once each. **The server may keep running**: phones keep backing up, and imported files reach them at
once (see "Several processes on one data folder"). Run from the server folder; try `--dry-run` first (it only
counts):

```
go run ./scripts/import --user hen --imported D:\Imported --not-imported "D:\Not imported" [--dry-run] [--move-other] D:\Backups "E:\Old phone"
```

- Each folder given is searched at any depth, except hidden folders (`.thumbnails`), `$RECYCLE.BIN`,
  `System Volume Information`, the data folder and the imported folder. Hidden files (Android's `.trashed-…`)
  and files that aren't photos or videos are counted, not touched.
- Each file is hashed; one whose sha256 the data folder already has (an earlier run, a device, or another copy
  found in this run) is a **duplicate** and isn't copied. A new file is copied to `.cache/import/`, hashed again,
  and placed like an upload (month folder, safe name, sidecar), with `source` from its folder like the Android
  app (`camera`, `screenshots`, `whatsapp`, `other`), or `whatsapp` when WhatsApp named it
  (`IMG-20230115-WA0012.jpg`) wherever it was found (`media.SourceOfPath`).
- **Every source file that is done, copied or duplicate, is moved to a folder of this run inside `--imported`**,
  named by when the run started, keeping its path from the searched folder down
  (`D:\Archive\phone DCIM\Camera\x.jpg` → `D:\Imported\2026-10-20-19-15-42\phone DCIM\Camera\x.jpg`), so runs
  never mix or overwrite each other and each can be deleted once its result looks right (with the run's
  `dates.tsv` and `near-duplicates.tsv`).
- **Files that weren't imported are moved to `--not-imported`**, a separate folder so deleting an imported run
  never deletes them: `<not-imported>\<run time>\<why>\phone DCIM\…`, with `<why>` one of `no date`, `failed`,
  `near-duplicates` (waiting for `--review`), and with `--move-other` also `not photos or videos` and `hidden`
  (`.trashed-…`); off by default, so searching a whole drive never moves its other files. The run's
  `not-imported.tsv` lists each with its reason, where it was found and where it is now. Only files that can't
  be moved stay where they are (`left-where-they-are.tsv`). Running again continues where it stopped (also after
  Ctrl+C); neither folder is ever searched.
- **Several imports may run at once**, next to the server (different source folders, a cloud import, `--review`);
  each claims its own run folder (`…-2` when two start in the same second), and the review list is changed by one
  at a time. See "Several processes on one data folder".
- **Files are read several at a time, then decided one at a time, largest first**, so the original of a photo
  goes in before its smaller copies (which wait as near-duplicates), whatever order they were read in. An
  identical copy of a waiting near-duplicate is moved to the imported folder as its duplicate; its folder is
  kept with the waiting photo, which is decided on once.
- **Files are only copied**, byte for byte (the copy's sha256 is checked); nothing is written into them. What is
  read from them goes in the sidecar, like an upload's facts.
- **Dates come from the file name first** (`20251202_222720.mp4`, `PIC_20251202_192632357.jpg`,
  `Screenshot_20251201_093154_App.png`): the metadata is used only when it shows the same local time (it adds the
  offset) or when the name has no date. For photos that is EXIF `DateTimeOriginal` with `OffsetTimeOriginal`, read
  with `vipsheader`, or from the file itself for DNG (vipsheader shows no EXIF for them). For videos it is Apple's
  `com.apple.quicktime.creationdate`, or ffprobe's `creation_time` (UTC), from which the offset is worked out against
  the name's local time. A time without an offset is in the server machine's time zone. Name and metadata more
  than a day apart are listed in `dates.tsv`. Files with no date stay where they are.
- Width and height (upright), length, and GPS location (EXIF, or a video's ISO 6709 `location` tag) are read too.
- **Folders are kept as clues.** The data folder stays arranged by date (`media/YYYY/YYYY-MM/`); the folder each
  file was found in, from the searched folder down (`phone DCIM/Flowers`, `phone DCIM/2026-06/2025`), goes in the
  sidecar's `folders`, as found. A duplicate adds its folder to the copy already in the data folder (often the
  copy kept in an album is the duplicate), and so does a near-duplicate skipped on the review page. Only the
  sidecar is rewritten. `--backfill-folders <folder>…` adds the folders of files looked at by runs from before
  this, from `.cache/source-hashes.tsv`.
- **Near-duplicates** (photos that look alike but aren't the same bytes: a resized copy, a burst) are found by
  **dHash** (the photo shrunk by libvips to 9×8 grey pixels, one bit per pair of neighbors) and listed in
  `near-duplicates.tsv` when within `--near` bits (default 4) of a photo in the data folder or imported
  before. **Shots of a burst** (taken 1–60 s apart, or in the same second with different burst numbers,
  `20260120_081253_035.jpg` / `…_041.jpg`) are imported. **Any other near-duplicate is left where it is** and
  added to `<imported>/review.json`; `--review` opens a page on this machine showing each next to the photo it
  looks like, with both photos' facts (rows that differ highlighted), to **import it too** or **skip it** (moved to
  the imported folder like a duplicate; decisions are logged in that run folder's `review-decisions.tsv`; `review.json` stays at the top of `--imported`, across runs). The dHash of every photo
  in the data folder is kept in `.cache/dhash.tsv` (by sha256), and the sha256 of every file looked at in
  `.cache/source-hashes.tsv` (by path, size and modified time), so later runs don't redo them. Videos have no dHash.

### Importing from Google Drive and OneDrive

```
go run ./scripts/import --user hen --imported D:\Imported --not-imported "D:\Not imported" --cloud gdrive,onedrive --temp D:\CloudTemp [--dry-run] [--batch-gb 20]
```

Through **rclone** (a separate program: `winget install Rclone.Rclone`, `brew install rclone`). The first time for
each service, rclone opens the browser to sign in, with **read-only** access (remotes `journal-gdrive` and
`journal-onedrive` in rclone's own config); nothing in the cloud is ever changed. Then:

- Every photo and video is listed (`rclone lsjson --hash`). Files handled by earlier runs are skipped
  (`.cache/cloud-seen.tsv`, by cloud path, size and modified time, which only tells whether a file changed). On
  **Google Drive**, which reports each file's sha256, a file the data folder already has is **not downloaded**:
  its folder is added to the copy there.
- The rest are downloaded to `--temp` in batches of about `--batch-gb`, largest first, and imported like files on
  disk (same dates, duplicates, near-duplicates, folders: `Google Drive/Photos/2019 Italy`). `importedFrom` is the
  cloud path (`Google Drive:Photos/2019 Italy/x.jpg`). Each file is then **deleted from `--temp`**; one not
  imported is moved to `--not-imported` as usual.
- **Every file's outcome is appended to `<imported>/cloud-log.tsv`**: time, result (imported, duplicate, not
  imported, near-duplicate waiting for review, download failed), cloud path, sha256, its path in the data folder.
  Failed downloads are tried again next run.
- `--dry-run` only lists: how many would be downloaded, and how many Google Drive files are already in the data
  folder. Ctrl+C stops; files already downloaded stay in `--temp` and the next run imports them.
- Google Photos can't be read by programs since March 2025: use Google Takeout, then import its folders.

## Several processes on one data folder

The server, `scripts/import` (one or more at once), `scripts/scan-nudity` and `scripts/fix-sources` may all have the same data folder
open. Each keeps the index in memory, so they **take turns** at the few steps that change it (`media/turns.go`):
placing a file (a finished upload, an import), and rewriting a sidecar (folders, a scan's answer, a corrected source).

- A turn holds the exclusive lock on `.cache/write.lock` (the operating system's file lock, dropped if the process
  dies). It first catches up with what the others did, from `.cache/write-journal.tsv` (`<sha256>\t<path>` per file
  placed or sidecar rewritten), then checks again that the file is new, picks its name and writes, and appends its
  own lines. A turn takes milliseconds: copying and hashing an import, or receiving an upload, happen before it.
- So a file is stored once even when a phone and an import bring it at the same moment (the second finds it
  there: `MediaExists`, or a duplicate), and two files of the same name never overwrite each other.
- Outside turns a store catches up before answering about the whole folder (`missingMedia`, `startUpload`,
  `changes`, `storageUsage`): an imported file reaches the phones in their next change feed, and a feed position
  can't pass a file placed by another process. A scan's `scannedAt` is the time of its turn, for the same reason.
- The journal only grows (about 100 bytes a file); a store opened later starts reading at its end, as the sidecars
  already hold everything before it.

## Timeouts

The server has no overall read or write timeout, because a piece on mobile data or a long video
stream takes as long as it takes. Instead: headers must arrive within 10 s; GraphQL requests get
30 s to arrive and 2 min to be answered; each upload piece gets 10 min to arrive.
