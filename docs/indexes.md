# Indexes and the reader

The server keeps several **indexes** of each user's photos and videos, each made by reading the file once:

| Index | Package | Reads | Keeps |
|---|---|---|---|
| nudity | `nudity` | the photo fitted in 320×320 | the score in the sidecar's `detected.nudity` (see [media.md](media.md)) |
| faces | `faces` | the photo fitted in 640 and in 1280, then each face cut from the original | `.cache/faces/<model>.faces` (see [faces.md](faces.md)) |
| search | `search` | the photo squeezed to 256×256 | `.cache/search/<model>.vectors` (see [search.md](search.md)) |

One **reader** (package `reading`) feeds them all. Each file is decoded **once**, and every index gets exactly the
picture its model reads, each **made from the original** at that size, never from another picture made before.

## How the reader works
- **What to read.** It looks through each user's files, newest upload first, for those any index still `Needs`: for
  example not scanned by this model, or no vector yet. It looks again after every upload (`Wake`) and every 30
  seconds (imports).
- **Decoding once.** libvips, inside the server ([media.md](media.md), "Previews"), decodes the original whole into
  memory, upright: a video's poster frame, or a JPEG XL DNG's developed raw data. From that it makes each index's
  pictures (`Original.Picture`: "contain" fits inside and never enlarges; "squeeze" stretches to exactly the model's
  square). Only the sizes still needed are made: if only search still needs a file, only its 256×256. A file no
  needing index wants a picture of is not decoded at all.
- **Two stages, overlapping.**
  - Stage 1 prepares the next files (decoding and resizing in libvips, on its own threads) while stage 2 runs the
    indexes' models on the file before.
  - The reader's rate is set by the slower stage, not by both added up: a HEIC's ~1 s decode happens while the models
    read the photo before it.
  - `"reader_prepare"` in config.json (default 2) is how many files are prepared ahead.
- **Order.** On each file the indexes run in their `Order`: nudity first (it hides), then faces, then search.
- **Memory.** A decoded original is 3 bytes a pixel: 36 MB for 12 MP.
  - At most `reader_prepare` + 1 originals are held, and only one over 50 MP.
  - One over 100 MP is decoded at twice the largest picture asked for, not whole.
  - An original is let go as soon as its pictures are made, unless an index cuts regions of it (faces). Then it is
    let go after the last index.
- **Failures.**
  - A file that can't be decoded fails every index that needed it; a model's error fails only that index.
  - Each index lists its own failures in `.cache/<index>/<model>.failed.log` in the data folder, one tab-separated
    line each: when (UTC), sha256, the file's path, and why.
  - Every failure is listed, also those not logged one by one: without libvips or ffmpeg every file fails alike,
    and the console says so once at start.
  - A file that failed is tried again only after a restart, so it appears once per restart. The lists can be
    deleted.
- **Progress.** One line every 1000 files, then a summary:

  ```
  reading: 1000 of 48210 files of hen (nudity 1000, faces 997 (3 could not be read), search 0; 2.9/s, about 4h30m left)
  ```

Measured on 20 of the owner's photos (Oct 23, 2026), against before (each index making its own preview with the vips
program), for all three indexes' pictures of one photo: **JPEG 600 → 117 ms, HEIC 1.92 s → 573 ms**.

## Adding an index
An index is one type that implements `reading.Index`:

```go
Name() string                    // "ocr": in logs and progress lines
Order() int                      // when several need a file, lower first (nudity 0, faces 10, search 20)
Wants() reading.Wants            // the kinds it reads, its pictures (media.PreviewSpec, each from the original), and
                                 // Regions when it cuts parts of the original itself (Input.Original.Region)
Needs(user string, item media.Item) bool          // still to read, by its present model
Read(ctx context.Context, user string, in reading.Input) error // read one file, keep the result itself
Finish(user string)              // after a run of reads (faces groups its new faces here)
FailureLog() (folder, name string) // .cache/<folder>/<name>
```

Then add it to the reader in `newReader` (`library.go`). Where it keeps its results:
- what a model found goes in the sidecar's `detected` (with `Store.SetNudity` as the pattern), so devices learn of it
  through the feed;
- anything bigger, or derived and rebuildable, goes in `.cache/<name>/` (as faces and search do), written only by the
  server.

The reader takes care of the rest: picture making, order, the memory budget, failures, progress, wake-ups and
restarts. An index that reads no picture (only the sidecar, say) costs no decoding.
