# Media: storage and upload

Devices upload originals to the server, which keeps them byte for byte in the user's **data folder**
as plain files. The files are the truth: the index in memory is rebuilt from the sidecars every
time the server starts, so the folder alone is a complete copy, on Windows, macOS or Linux.

Built so far: upload (resumable, deduplicated by sha256) and downloading or streaming originals.
**Not built yet:** previews (made by the server, at the size the client asks for), video posters,
sync of stories and notes (change feed), and the story trash. The agreed design is in
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

`query media(sha256)` returns the facts, plus `versions` (edits of this photo) and `paired` (files
that name it in `pairedWith`).

All of these need a client certificate, like everything except enrollment
(see [authentication.md](authentication.md)).

## Timeouts

The server has no overall read or write timeout, because a piece on mobile data or a long video
stream takes as long as it takes. Instead: headers must arrive within 10 s; GraphQL requests get
30 s to arrive and 2 min to be answered; each upload piece gets 10 min to arrive.
