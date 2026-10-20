# Media: storage and upload

Devices upload originals to the server, which keeps them byte for byte in the user's **data folder**
as plain files. The files are the truth: the index in memory is rebuilt from the sidecars every
time the server starts, so the folder alone is a complete copy, on Windows, macOS or Linux.

Built so far: upload (resumable, deduplicated by sha256), downloading or streaming originals, and
photo previews at the exact size the client asks for, and the change feed for media. **Not built yet:**
video posters, stories and notes in the change feed, and the story trash. The server runs on **macOS** for now
(previews need libvips: `brew install vips`); Windows comes later. To run it on Windows now, run
`go run ./scripts/bundle-vips` once in the server folder: the official Windows builds of libvips (WinGet,
libvips.org) leave out the HEVC decoder and can't read iPhone HEIC photos, so this copies MSYS2's build,
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
a RAW (it names the JPEG). A `note` (caption and tags) will be added to the sidecar by sync.

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

## Previews

`GET /media/{sha256}/preview?w=1080&h=1440&fit=cover&crop=0.1,0.2,0.5,0.5&fmt=webp`

| Parameter | Meaning |
|---|---|
| `w`, `h` | The **physical pixels** the client will draw, 1–4096. The answer has exactly this size, so it is drawn 1:1 with no scaling on the device. |
| `fit` | `cover` (default): fills the box, centered, cutting off what doesn't fit. `contain`: fits inside the box, so one side may be shorter. |
| `crop` | Optional `x,y,w,h` as fractions of the **upright** photo (rounded to 1/10000): a story tile's focus and zoom. It is cut from the full-resolution original, so a 4× zoom stays sharp. |
| `fmt` | `webp` (default) or `jpeg`. |

- Made with libvips in one scaling step from the original (`vips thumbnail`, then `extract_area` when
  something is cut off). EXIF orientation is applied, and the colour profile (Display P3) is kept;
  other metadata is dropped. Quality 90.
- **Never scaled up.** When the box is bigger than the photo, the preview keeps the box's shape at the
  photo's own resolution (so it's smaller than asked); the client scales it on screen.
- Cached under `.cache/previews/` and served with `Cache-Control: immutable`. When a user's previews
  pass `preview_cache_max_gb` (default 50), the least recently used are deleted (checked hourly).
- Answers: `422` bad parameters, `404` unknown sha256, `501` for videos (posters not built yet), `503`
  when libvips isn't installed (the server logs this at startup; set `"vips"` in config.json if it's
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
deleted, media }`, oldest upload first, with a `cursor` and `hasMore`. A new device starts without `since` and calls
again with each page's cursor until `hasMore` is false; afterwards it asks with its saved cursor for what was uploaded
since (an empty page returns the same cursor). The cursor is opaque (today the upload time and sha256 of the last
original in the page), so stories and notes can join the feed later without changing clients. Pages are at most
5000; responses to `/query` are gzipped when the client accepts it.

## Timeouts

The server has no overall read or write timeout, because a piece on mobile data or a long video
stream takes as long as it takes. Instead: headers must arrive within 10 s; GraphQL requests get
30 s to arrive and 2 min to be answered; each upload piece gets 10 min to arrive.
