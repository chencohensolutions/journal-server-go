# Media: storage and upload

Devices upload originals to the server, which keeps them byte for byte in the user's **data folder**
as plain files. The files are the truth: the index in memory is rebuilt from the sidecars every
time the server starts, so the folder alone is a complete copy, on Windows, macOS or Linux.

Reading every sidecar takes minutes on a spinning disk (one seek each), so what was read is kept in
`.cache/index.json`, folder by folder, with each folder's modified time. On start only folders whose time has
changed are read again. Adding, removing or renaming a file changes its folder's time, and every write the server
and scripts make ends in a rename, so nothing they do is missed. A sidecar edited **in place by hand** is not
noticed: delete `.cache/index.json` and the next start reads everything.

The server keeps the cache up to date while it runs, so a restart reads only what changed since:
- every hour, catching the scripts' imports too;
- whenever it has written into the data folder itself, at most 5 minutes later, and after each run of the reader.
  The nudity scan rewrites a sidecar for every file it reads, which used to make the next start read much of the
  library again;
- when it is stopped with Ctrl+C. It then finishes the file being read, saves the cache and exits; a second
  Ctrl+C exits at once.

Built so far: upload (resumable, deduplicated by sha256), downloading or streaming originals, verifying an
original before a device deletes its copy, and photo previews at the exact size the client asks for, video posters, the change feed for media, and
search by what photos show. **Not built
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
  settings.json                             ← hidden collections (below)
  people.json                               ← the people the user named in face search (faces.md)
  .cache/index.json                         ← what the last scan read, by folder (only a shortcut)
  .cache/uploads/<sha256>.partial           ← an upload in progress
  .cache/uploads/<sha256>.json              ← what the device declared for it
  .cache/previews/<sha[:2]>/<sha256>/1080x1440-cover.webp   ← previews made so far
  .cache/search/<model>.vectors             ← text search's vectors (search.md)
  .cache/faces/<model>.faces                ← the faces found in each file (faces.md)
  .cache/ocr/<model>.jsonl                  ← the text read in each photo (indexes.md)
  .cache/similar/<model>.vectors            ← DINOv2 vectors, for similar photos and duplicates (indexes.md)
  .cache/aesthetic/<model>.vectors          ← each file's aesthetic score (indexes.md)
  .cache/{nudity,faces,search}/<model>.failed.log  ← files those indexes could not read, one line each
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
a RAW (it names the JPEG). A file named like a screenshot (`Screenshot_20240301_030715_Bumble.jpg`,
`Screenshot_2015-07-18-03-54-57.png`) gets `"source": "screenshots"` whatever the device or folder said, and
`app`, the app on screen, when the name has one (`"app": "Bumble"`, also from `…_Bumble(1).jpg` and
`…_Bumble~20221117-191459.jpg`; `media.ScreenshotApp`). A `note` (caption and tags) will be added to the sidecar by sync. `detected` holds what
models found in the file (see [Nudity scan](#nudity-scan)). Files brought in
by the import script have `"uploadedBy": "import"` and `importedFrom`, the path they were found at. `correctedAt`
is set when the server later corrects a fact clients show (see [Fixing sources](#fixing-sources)). `privacy` is
there once the user marked the file private (see [Private files](#private-files)). `kind` is `photo`, `video` or
`audio`; a sound recording may have `call` and `encodedFrom` (see [Sound recordings](#sound-recordings)).

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
| `400`, `Upload-Offset: n` | The piece ended early; what arrived is kept | Continue from `n` |
| `400` without `Upload-Offset` | Bad `Content-Range`, or `Content-Length` doesn't match it | Fix it (a client bug) |
| `404` | No upload started for this sha256, or it expired | `startUpload` again |
| `409` | The bytes don't have the declared sha256; the upload is deleted | Hash the file again and start over (it may have been edited meanwhile) |
| `413` | Piece larger than 64 MB | Send smaller pieces |
| `416`, `Upload-Offset: n` | The piece doesn't start where the received bytes end | Continue from `n` |

- **Resuming:** `HEAD /upload/{sha256}` returns `Upload-Offset` (and `Upload-Complete`). `startUpload`
  again also returns the offset. Either works after a dropped connection or a server restart.
- **Pieces:** the server asks for 16 MB (`chunkSize`) and accepts up to 64 MB. The bytes of a piece that breaks
  off are kept (they arrive in order, and the whole file is checked at the end), so the next piece starts where they
  ended; pieces needn't line up with anything.
- **Two devices, one file:** each sha256 is written by one request at a time, and the second device's
  `startUpload` reports `MediaExists` once the first has finished.
- **Expiry:** an unfinished upload is deleted 7 days after its last `startUpload` (checked hourly).
- **Rejections:** `UNKNOWN_ORIGINAL` / `UNKNOWN_PAIR` (upload that file first), `TOO_LARGE` (over
  32 GB), `NO_SPACE` (the upload would leave less than 1 GB free on the server's disk).

## Downloading and streaming

`GET /media/{sha256}` returns the original with its MIME type, `ETag: "<sha256>"`, and
`Cache-Control: private, max-age=31536000, immutable`, since the content of a sha256 never changes.
It supports `Range`, which is how videos and sound recordings stream and seek; neither is ever converted.

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

- Made with libvips in one scaling step from the original (`thumbnail`, then `extract_area` when
  something is cut off). EXIF orientation is applied, and the colour profile (Display P3) is kept;
  other metadata is dropped. Quality 90.
- **libvips runs inside the server** (`media/vipslib*.go`, since Oct 23, 2026). It is loaded once at start, the
  same library `vips.exe` runs (`libvips-42.dll` in `vips/bin`, Homebrew's `libvips.42.dylib`, or Linux's
  `libvips.so.42`), and called through purego like ONNX Runtime, with no cgo.
  - **Why:** starting `vips.exe` and `vipsheader.exe` cost about 350 ms each on Windows before any work. On 20 of
    the owner's photos, a grid preview went from 365 to 219 ms (JPEG) and 827 to 673 ms (HEIC).
  - **Only fixed-argument calls are used:** an operation is made by name, its properties set one by one, then
    built, because variadic C calls break on Apple Silicon.
  - **On Windows the server searches `vips/bin` for DLLs first**, so libvips's HEIF and JPEG XL modules (loaded
    later) never pick up another libvips on the PATH, such as the official build, whose libheif has no HEVC.
  - **The programs stay as the fallback:** when the library can't be loaded, and for a file it failed on (until a
    restart).
  - **The risk:** a crash in libvips now takes the server down. `"vips_as_program": true` in config.json keeps
    libvips out of the server, and `"libvips"` names the library's path.
- **DNG (RAW)** previews show the raw data, never the JPEG a DNG carries. Most DNGs are read by libvips
  (LibRaw). Samsung's Expert RAW and Pro mode DNGs (DNG 1.7: demosaiced "linear" data compressed with JPEG XL,
  which LibRaw can't unpack) are developed by the server (`media/develop.go`): libvips decodes the JPEG XL, then
  the DNG's own colour processing is applied (black and white levels, white balance and its ColorMatrix /
  ForwardMatrix to sRGB, BaselineExposure, the ProfileGainTableMap that lifts the shadows, a soft roll-off of
  highlights, its default crop and orientation). Inside the server this happens in memory: each JPEG XL tile is
  decoded from the DNG's own bytes, the tiles are joined, and the developed pixels go straight into the preview,
  with no temporary files. The result is kept full size as `developed.jpg` beside the photo's previews when the
  user's `preview_cache` is on (about 2.5 s for a 24 MP photo, so with the cache off every preview of such a DNG
  takes that long).
  libvips needs JPEG XL support for this: Homebrew's has it, and so does the Windows bundle.
- **Never scaled up.** When the box is bigger than the photo, the preview keeps the box's shape at the
  photo's own resolution (so it's smaller than asked); the client scales it on screen.
- Served with `Cache-Control: immutable`; devices keep each size they ask for, and ask for a size only when they
  have neither it nor the original. **Kept on the server only with `"preview_cache": true`** in the user's entry in
  `config.json` (off by default): then under `.cache/previews/`, and when they pass `preview_cache_max_gb` (default
  50) the least recently used are deleted (checked hourly). Without it each preview is made in a temporary folder,
  sent and deleted.
- **Videos** get their poster: the first keyframe at 1 s or later (halfway through a shorter video), taken by
  **ffmpeg** with the video's rotation applied, HDR (HLG or PQ) tone-mapped to standard range, then made into the
  preview like a photo.
  - **Why a keyframe:** ffmpeg decodes only keyframes, instead of every frame from the keyframe before 1 s.
    The owner's phone puts a keyframe every 1.1 to 1.6 s, and a 4K HDR poster went from about 3.7–4.4 s to 1.3–1.7 s
    (Oct 23, 2026).
  - **Why 1 s in:** the very first frame, though always a keyframe, is often black or blurry while the camera
    settles.
  - **When there's no keyframe there** (a short video), the first frame is taken.
  ffmpeg is found like libvips (`"ffmpeg"` in config.json, the PATH, or Homebrew; on Windows `winget install
  Gyan.FFmpeg`). The frame is kept as `poster.jpg` only with `preview_cache` (about 2 s for a large video on a disk).
- Answers: `422` bad parameters, `404` unknown sha256, `503` when libvips (or, for a video, ffmpeg) isn't installed (the server logs this at startup; set `"vips"` in config.json if it's
  installed somewhere unusual).

`query media(sha256)` returns the facts, plus `versions` (edits of this photo) and `paired` (files
that name it in `pairedWith`).

`query storageUsage` returns how many photos, videos and sound recordings (`audio`) the user has and the bytes
their originals take (`photoBytes`, `videoBytes`, `audioBytes`, `bytes`), counted from the index, so it answers at once. Sidecars, uploads in
progress and previews aren't counted. The app shows it in Settings → Backup.

All of these need a client certificate, like everything except enrollment
(see [authentication.md](authentication.md)).

## Change feed

`query changes(since: String, limit: Int = 2000)` returns pages of `Change { kind: MEDIA,
path: "media/<sha256>", deleted, hidden, media }`, oldest change first, with a `cursor`, `hasMore`, and `remaining` (how many
changes come after the page, for a progress count). A new device starts without `since` and calls again with each
page's cursor until `hasMore` is false; afterwards it asks with its saved cursor for what changed since (an empty page
returns the same cursor). A file changes when it is uploaded, and again when a scan writes its answer into the sidecar
(see [Nudity scan](#nudity-scan)), its source is corrected (see [Fixing sources](#fixing-sources)) or it is marked
private or unmarked (see [Private files](#private-files)): it then comes again, whole. To a device the server keeps
from private files, a private file comes as `deleted`, and so does a file the user hides (one of the
[hidden collections](#hidden-collections), or nudity) to every device, with `hidden` set, so the device leaves out its
own copy too. Pages are at most 5000; responses to `/query` are gzipped
when the client accepts it.

- **The cursor** is opaque. It carries where the device is in the feed (the time of the last change and sha256 of
  the last original in the page) and the view it was given out for: whether private files were withheld, and what
  was hidden. Stories and notes can join the feed later without changing clients. A cursor is never wrong to send:
  one the server can't read (a format from another server, or damaged) starts the feed again from the beginning,
  with hidden files as `deleted`, since what the device holds is unknown.
- **When the view changes** (the device's private setting, or the user's hidden collections), the next sync gets
  only the files whose visibility changed: the ones now hidden as `deleted`, the ones shown again whole, in pages
  (`remaining` counts them), and then the feed goes on where it was. Everything else stays as the device has it. If
  the view changes again before that change-over is read to the end, the feed starts again from the beginning.

## Nudity scan

The server scores every photo and video for nudity with [NudeNet](https://github.com/notAI-tech/NudeNet)'s
detector (`320n.onnx`, a small YOLOv8 model, AGPL-3.0) on ONNX Runtime, the same runtime as search and faces, with
no Python (package `nudity`). `go run ./scripts/bundle-onnx` downloads it to `models/nudenet/320n.onnx`
(`nudity_model` in config.json); without it nothing is scanned, the server says so at start, and
`serverInfo.nudityScan` is false. It writes the answer into the sidecar:

```json
"detected": {
  "nudity": { "score": 0.93, "model": "nudenet-320n", "scannedAt": "2026-10-20T19:24:10Z" }
}
```

- The score is NudeNet's highest confidence in an exposed breast, genitals, buttocks or anus; 0 when it finds none.
- It is never a tag: the user's tags stay theirs. The API gives it as `Media.nudity` (null until scanned).
- **The server hides what scores high** on every device, like a hidden collection (see
  [Hidden collections](#hidden-collections)): from 0.5 until the user chooses another score or turns it off. A file
  not scanned yet isn't hidden, so a new file shows until the scan has read it.
- **It runs by itself**, the first index of the server's reader ([indexes.md](indexes.md)): the files not scanned
  yet, newest upload first, then each upload as soon as it finishes and imports within 30 seconds. A file that can't
  be scanned is tried again after the server restarts, and listed in `.cache/nudity/nudenet-320n.failed.log` (when,
  sha256, path, why).
- It reads the photo fitted in 320×320, made from the original (a video's poster frame), and does what NudeNet's own
  `detect` does: pads it with black to a square at the right or bottom (320×320, so NudeNet's own resize changes
  nothing), gives it as blue, green and red planes of 0 to 1 (NudeNet's order), keeps each box whose
  surest label reaches 0.2, and of boxes that overlap by more than 0.45, whatever their labels, the surest over 0.25.
- `go run ./scripts/scan-nudity --user hen` scans a data folder without the server (or beside it: see "Several
  processes on one data folder"), and prints what scores 0.5 or more. Files already scanned by the same `model` are
  skipped, so a stopped run continues where it stopped; `--rescan` scans everything again.

## Private files

The user can mark files private from the apps: `mutation setPrivate(sha256s: [Sha256!]!, private: Boolean!)`, at
most 1000 per call. It returns the files that changed. The mark goes in the sidecar, beside the facts:

```json
"privacy": { "private": true, "setAt": "2026-10-21T12:19:09Z", "setBy": "Pixel 9 Pro" }
```

- It is the user's choice, so it is never in `detected` (model output). Unmarking keeps the field with
  `"private": false` and a new `setAt`, so the change comes again in the change feed like any other.
- A file is marked with everything that goes with it, through `versionOf` and `pairedWith` either way: the photo and
  its edits, a Live Photo's video, a RAW and its JPEG. An edit or paired file uploaded later of a private file is
  private from the start (`setBy` is the device that uploaded it).
- The API gives it as `Media.private`. **Clients hide private files themselves**: not shown while "Show private
  media" is off (the default), and nothing of them is fetched. `searchMedia`, `people` and `similarFaces` leave them
  out unless asked to include them (`includePrivate`).

**Keeping private files from a device.** By default every device gets private files, flagged. A device can be kept
from them entirely, for example a tablet others use: `journal-server private <serial> withhold` (`allow` undoes it;
`journal-server devices` shows which are withheld). This is `withhold_private` in `devices.json`. Such a device:

- doesn't get private files in its change feed. A file marked private after the device had it comes as `deleted`, so
  the app drops it. Changing the setting makes the device's next sync take back, or get again, exactly the private
  files it already had (its cursor says which way it was given; see [Change feed](#change-feed)).
- gets `null` from `media(sha256:)`, and `404` from `/media/{sha256}` and its `/preview`, as if the server didn't
  have the file. Edits and paired files it can't see are left out of `versions`, `paired`, `versionOf` and
  `pairedWith`.
- may mark files private but not unmark them (it can't see them).
- still learns that the server has a private file it holds itself: `missingMedia` doesn't list it, `startUpload`
  answers `MediaExists`, and `/verify` checks it, so it isn't uploaded again and its own copy can be freed. It learns
  nothing it didn't have. Its own copies of private files on the phone are not hidden by the server.
- Certificates made outside enrollment are not in `devices.json` and always get private files.

Withholding protects what a lost device holds better than the apps' hiding, since nothing private reaches it. Revoke a
lost device in any case (see [authentication.md](authentication.md)).

## Hidden collections

The user can hide whole groups of files on every device, without deleting them (Settings → Private media → Hidden
collections in the apps): all of WhatsApp, the screenshots of chosen apps (by `facts.app`, compared ignoring case),
screenshots whose name says no app, the files the [nudity scan](#nudity-scan) scored at a chosen score or more,
and [sound recordings](#sound-recordings): recordings, calls and voice messages, each hidden until the user shows it.
`query hiddenCollections` lists them with how many files each has, hidden or not, apps with most files first (a
hidden app with no files left is listed too, so it can be shown again), and for nudity how many files score that
much and how many aren't scanned yet. `mutation setHiddenCollections(input:)` replaces the whole choice (its
`nudity` and `audio` may be left out to keep them) and answers the same list. The choice is the user's, not a device's, and is
kept at the data folder's root, so it moves with the data:

```json
// settings.json
{ "format": 1, "hidden": { "sources": ["whatsapp"], "screenshotApps": ["Bumble", "OkCupid"], "screenshotsWithoutApp": true, "nudityFrom": 0.5,
             "audio": { "recordings": true, "calls": false, "voiceMessages": true } } }
```

`nudityFrom` is the score from which files are hidden, `0` for none. A data folder without settings.json, or one
written before the server hid nudity (no `nudityFrom`), hides from 0.5, as the apps used to do themselves. Likewise
without `audio` every sound recording is hidden.

- **The server does the hiding**, as for a device kept from private files: a hidden file comes in the change feed as
  `deleted` and `hidden` (also in a feed from the start, for the phone's own copy; see below), `media(sha256:)` is `null`, `/media/{sha256}` and
  its preview answer 404, `storageUsage` doesn't count it, and `searchMedia`, `people` and `similarFaces` don't find
  or count it. An edit or paired file of a hidden file is hidden with it. The apps drop it like a deleted one, with
  their cached previews and originals of it.
- **Nothing is deleted or rewritten**: originals and sidecars stay as they are. Showing a collection again brings
  its files back.
- A file the scan scores later comes again in the feed (its sidecar changed), as hidden if it scored that much.
- Changing the choice makes every device's next sync drop the files now hidden and get back those shown again, and
  nothing else (the cursor carries what was hidden when it was given out; see [Change feed](#change-feed)).
- `missingMedia`, `startUpload`, uploads and `/verify` still see hidden files, so a phone never uploads one again
  and can still free its own copy. The server can't reach the phone's own copies in its gallery: `hidden` tells the
  app to leave out its copy with that sha256 too (once backup has read it).

## Sound recordings

Recordings of events, recorded phone calls and WhatsApp voice messages are files of `kind` `audio` (GraphQL
`AUDIO`), brought in by [the import](#importing-files-already-on-the-servers-disk). They sit in the month folder of
when they started, like any file. Each is in one of three [hidden collections](#hidden-collections), all hidden on
every device until the user shows them (`media.AudioCollectionOf`):

- **calls**: a sidecar with `call`, who the call was with, as the recorder named the file. Samsung names a call
  after the contact or, without one, the number, so usually only one is known:

  ```json
  "call": { "contact": "אבא" }      // or { "number": "0501234567" }, or {} (Memento's Audio/Calls)
  ```

  `mutation setCall(sha256:, call: {contact, number})` corrects it (both are replaced; empty is none), and makes
  a recording set so a call. It sets `correctedAt`, so the file comes again in the change feed. `call` is the
  device's to show, never a model's.
- **voiceMessages**: `source` `whatsapp` (`PTT-20250208-WA0004.opus`, `AUD-…`).
- **recordings**: any other.

`takenAt` is when it started and `durationMs` its length; GraphQL's `Media.endedAt` is the two added up, so an app
can show where a recording starts and ends and the photos taken in between. A WAV is kept as FLAC (the same sound in
about half the bytes); its sidecar's `encodedFrom` names the WAV, which isn't kept:

```json
"encodedFrom": { "sha256": "4be1…09", "mimeType": "audio/wav", "size": 51732524 }
```

so importing that WAV again finds the FLAC (`Store.EncodedFrom`). A sound recording has no picture:
`/media/{sha256}/preview` answers 404, and no index reads it (nudity, faces, text search, OCR, similar photos and
aesthetic scores all list the kinds they read). It plays from `/media/{sha256}` with `Range`, as a video does.
`serverInfo.audio` says the server knows sound recordings.

## Search

Files can be found by a description in words with `searchMedia(text: "dog on the beach")`. It runs a SigLIP 2 model
with ONNX Runtime inside the server, no Python. See [search.md](search.md).

## Faces

Files can be found by the people in them: the server finds the faces in each file, puts alike faces in groups, and
the user names a group once (`people`, `namePerson`, `searchMedia(people: […])`, `similarFaces`). Two small open
models, YuNet and SFace, run on the same ONNX Runtime. See [faces.md](faces.md).

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
- It also gives screenshots placed before the server read the app from their names what a new one gets
  (`"source": "screenshots"` and `app`; see [Sidecar](#sidecar-json)), and prints how many it corrected for each app.
  Only `facts.source`, `facts.app` and `facts.correctedAt` change.

## Importing files already on the server's disk

`scripts/import` copies photos, videos and sound recordings from folders on the server's disk (old phone backups)
into a user's data folder, once each. **The server may keep running**: phones keep backing up, and imported files reach them at
once (see "Several processes on one data folder"). Run from the server folder; try `--dry-run` first (it only
counts):

```
go run ./scripts/import --user hen --imported D:\Imported --not-imported "D:\Not imported" [--dry-run] [--move-other] D:\Backups "E:\Old phone"
```

- Each folder given is searched at any depth, except hidden folders (`.thumbnails`), `$RECYCLE.BIN`,
  `System Volume Information`, the data folder and the imported folder. Hidden files (Android's `.trashed-…`,
  Samsung's `.evr_recently_deleted_…`) and files that aren't photos, videos or sound recordings are counted, not
  touched. The sound formats are those found so far, more to be added as they turn up: `.m4a`, `.aac`, `.flac`,
  `.ogg`, `.opus` and `.wav`.
- Each file is hashed; one whose sha256 the data folder already has (an earlier run, a device, or another copy
  found in this run) is a **duplicate** and isn't copied. A new file is copied to `.cache/import/`, hashed again,
  and placed like an upload (month folder, safe name, sidecar), with `source` from its folder like the Android
  app (`camera`, `screenshots`, `whatsapp`, `other`), or `whatsapp` when WhatsApp named it
  (`IMG-20230115-WA0012.jpg`) wherever it was found (`media.SourceOfPath`). Screenshots get their `app` from
the name, as uploads do.
- **Every source file that is done, copied or duplicate, is moved to a folder of this run inside `--imported`**,
  named by when the run started, keeping its path from the searched folder down
  (`D:\Archive\phone DCIM\Camera\x.jpg` → `D:\Imported\2026-10-20-19-15-42\phone DCIM\Camera\x.jpg`), so runs
  never mix or overwrite each other and each can be deleted once its result looks right (with the run's
  `dates.tsv` and `near-duplicates.tsv`).
- **Files that weren't imported are moved to `--not-imported`**, a separate folder so deleting an imported run
  never deletes them: `<not-imported>\<run time>\<why>\phone DCIM\…`, with `<why>` one of `no date`, `failed`,
  `near-duplicates` (waiting for `--review`), `same picture` (a copy of a photo in the journal), and with `--move-other` also `not photos or videos` and `hidden`
  (`.trashed-…`); off by default, so searching a whole drive never moves its other files. The run's
  `not-imported.tsv` lists each with its reason, where it was found and where it is now. Only files that can't
  be moved stay where they are (`left-where-they-are.tsv`). Running again continues where it stopped (also after
  Ctrl+C); neither folder is ever searched.
- **Several imports may run at once**, next to the server (different source folders, a cloud import, `--review`);
  each claims its own run folder (`…-2` when two start in the same second), and the review list is changed by one
  at a time. See "Several processes on one data folder".
- **Files are read several at a time, then decided one at a time, largest first**, so the original of a photo
  goes in before its smaller copies (found to be the same picture), whatever order they were read in. An
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
- **Sound recordings** (`audio.go`) are dated by their name: `recording-20220925-195923.flac` (what follows a `~` is
  not when it ended), `20250207 152917 12.aac`, `2020-04-10 00.02.41.ogg`, `PTT-20250208-WA0004.opus` (noon), and
  Samsung's calls, `Call recording <contact or number>_250206_142813.m4a` (also `הקלטת שיחה …` and `שיחה …`), which
  give `call` too. Android writes a recording's `creation_time` when it stops, so the start is that less the length;
  when it shows the name's local time, it gives the offset (Samsung also writes `com.samsung.android.utc_offset`).
  Without a date in the name, that start is used; without either, the file goes to `no date`. One ffprobe can't
  read (Samsung's newer `שיחה …` calls copied before they were finished have no `moov`) goes to `failed`. The MIME
  type is the format ffprobe finds, as some recorders write MP4 into `.aac` files. A file in a folder
  named `Call` or `Calls` with no contact in its name is a call with no one named. **A WAV is encoded as FLAC**
  with ffmpeg (metadata kept), in `.encoding` inside the run's not-imported folder, and the FLAC is imported; the WAV
  is then moved to the imported folder like any original. The summary counts recordings, calls and voice messages,
  and the WAVs kept as FLAC. Sound recordings have no dHash.
- Width and height (upright), length, and GPS location (EXIF, or a video's ISO 6709 `location` tag) are read too.
- **Folders are kept as clues.** The data folder stays arranged by date (`media/YYYY/YYYY-MM/`); the folder each
  file was found in, from the searched folder down (`phone DCIM/Flowers`, `phone DCIM/2026-06/2025`), goes in the
  sidecar's `folders`, as found. A duplicate adds its folder to the copy already in the data folder (often the
  copy kept in an album is the duplicate), and so does a near-duplicate skipped on the review page. Only the
  sidecar is rewritten. `--backfill-folders <folder>…` adds the folders of files looked at by runs from before
  this, from `.cache/source-hashes.tsv`.
- **Near-duplicates** (photos that look alike but aren't the same bytes) are found by **dHash** (the photo
  shrunk by libvips to 9×8 grey pixels, one bit per pair of neighbors): those within `--near` bits (default 8)
  of a photo in the data folder or imported before, up to 8 of them, closest first. Looking alike is not enough
  to hold a photo back: the **same-picture check** (`same.go`) tells a copy (resized, recompressed, its EXIF
  changed) from another shot (a burst, a second try):
  - **Same camera, another moment.** When both have the same EXIF `Make` and `Model` and a `DateTimeOriginal`,
    photos taken more than 10 ms apart are two shots. The milliseconds come from `SubSecTimeOriginal`, or from
    the name (`20260120_081253_035.jpg`, `PIC_20251202_192632357.jpg`); without them, a different second is
    enough. Without camera tags on both (WhatsApp strips them) the pixels decide.
  - **Names of two shots.** A copy keeps its name, give or take a mark (`(1)`, ` (2)`, `-נערכה` or `-edited`,
    `~20230328-021353`, `_saved`). Names with a date and time that differ (more than 10 ms when both have
    milliseconds) are two shots, such as two screenshots of one app. Names alike but for their digits are a
    camera's numbers for two shots (`DSC_000429` / `DSC_000430`, `…_084736_05` / `…_03`). WhatsApp's names are
    left out: it numbers every file it saves, so a photo sent twice has two numbers.
  - **Same shape.** Width-to-height ratios more than 1% apart (a crop) are two pictures.
  - **Same pixels.** Both are shrunk to 64×64 grey and compared (PSNR): 35 dB or more is the same picture, 28
    to 35 not sure, less another shot.

  **Another shot is imported.** **The same picture, no larger than the one in the journal, is not**: it goes to
  `<not-imported>\<run time>\same picture\…`, and its folders are added to that photo's. **A larger copy of
  it, or a pair the check isn't sure of, is left where it is** and added to `<imported>/review.json` (with
  `why` and `score`); `--review` opens a page on this machine showing each next to the photo it looks like,
  with both photos' facts (rows that differ highlighted), to **import it too** or **skip it** (moved to the
  imported folder like a duplicate; decisions are logged in that run folder's `review-decisions.tsv`;
  `review.json` stays at the top of `--imported`, across runs). `near-duplicates.tsv` lists every pair with the
  pixels' score, the dHash distance, the decision and why, sorted by score so the pairs near the thresholds sit
  together. **`--recheck [--dry-run]`** decides again, with the check, on every photo waiting in `review.json`
  (those held back before it, when looking alike was enough, too): other shots are imported and moved to the
  imported folder, the same pictures to `same picture`, and the rest keep waiting. The dHash of every photo in
  the data folder is kept in `.cache/dhash.tsv` (by sha256), and the sha256 of every file looked at in
  `.cache/source-hashes.tsv` (by path, size and modified time), so later runs don't redo them. Videos have no
  dHash.

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

## What the server can do

`query serverInfo { api version search previews posters nudityScan }` says what works on this server, so an app
shows only that: `search` (text search is on), `previews` (libvips is installed), `posters` (ffmpeg is installed),
`nudityScan` (new files are scanned for nudity, so hiding nudity works), `audio` (sound recordings can be in the
library), the
server's build (`version`: commit and date, or `dev`), and `api`, a number raised when a change needs newer apps
(1 today). An app asks once when it connects, rather than learning from errors.

## Errors

A GraphQL error a device should act on carries a code in `extensions.code`; the message is for people. Other errors
(a client bug, a server problem) have no code, and an app shows them as a server problem.

| Code | When | What the device does |
|---|---|---|
| `UNAUTHORIZED` | No client certificate for a field that needs one | Register again |
| `NO_DATA_FOLDER` | The user has no `data_folder` in `config.json` | Tell the user; the server's owner must set it |
| `INVALID_ARGUMENT` | An argument is over its limit (hashes, limit, offset, wait, years) or malformed | A client bug: fix the app |
| `SEARCH_OFF` | Text search is off on this server (`serverInfo.search` says so beforehand) | Hide search |
| `WITHHELD` | This device is kept from private files, so it can't unmark them | Tell the user |
| `INDEXING_OFF` | The server reads no files for indexes (no model), so there is no run to start (`serverInfo.indexing` says so beforehand) | Hide On your server |

The media routes answer with HTTP status codes instead, listed with each route above.

## Connections

The server offers **HTTP/2** and still speaks HTTP/1.1 (`tlsConfigFor` in `serve.go`). A device keeps one
connection, with one mTLS handshake (a signature in its secure hardware), and fetches previews, pages of the feed
and upload pieces side by side on it; OkHttp and URLSession choose it by themselves. An upload piece may have
3 MB in flight before the server acknowledges it (HTTP/2 flow control; Go's default of 1 MB would cap a piece at
about 20 MB/s over a 50 ms link). Idle connections are closed after 2 minutes.
