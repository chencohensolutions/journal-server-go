# Indexes and the reader

The server keeps several **indexes** of each user's photos and videos, each made by reading the file once:

| Index (order) | Model | Reads | Keeps |
|---|---|---|---|
| nudity (0) | `nudenet-320n` | the photo fitted in 320×320 | the score in the sidecar's `detected.nudity` (see [media.md](media.md)) |
| faces (10) | `yunet-sface` | the photo fitted in 640 and in 1280, then each face cut from the original | `.cache/faces/<model>.faces` (see [faces.md](faces.md)) |
| ocr (15) | `paddleocr-hebrew` | the photo fitted in 1280, then each word cut from the original at 48 px high | `.cache/ocr/<model>.jsonl` |
| search (20) | `siglip2-base-patch16-256` | the photo squeezed to 256×256 | `.cache/search/<model>.vectors` (see [search.md](search.md)) |
| similar (25) | `dinov2-small` | the photo's centre at 224×224 | `.cache/similar/<model>.vectors` |
| aesthetic (30) | `clip-vit-large-patch14-laion` | the photo's centre at 224×224 | `.cache/aesthetic/<model>.vectors` (one number a file) |

The console names each index by its model: `reading: 1000 of 48210 files of hen (nudenet-320n 1000, yunet-sface 997,
…)`. Each index is off when its model isn't there (`go run ./scripts/bundle-onnx` downloads them all; `--ocr=false`
and the like leave one out), and `serverInfo` tells the apps which are on.

**Cost (Oct 23, 2026).** On the owner's PC, with the server indexing at the same time:
- 22 photos took **3.8 s each** with all six indexes.
- Of that, CLIP ViT-L/14 is about 0.6 s, DINOv2 about 0.1 s, and OCR 0.3 s for a photo without text. A screenshot
  full of text takes several seconds more, at about 0.4 s a word.
- All the models loaded take about **2.2 GB** of memory: SigLIP about 1.5 GB, CLIP L/14 300 MB (8-bit), the others
  under 100 MB each.

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

## Indexing once a day (optional)
With `"index": "daily"` in config.json (the default is `"server"`), files are read by a separate process once a
day, and the server keeps only what queries need, so it takes far less memory.
- **The run:** `journal-server index` loads every index's model, reads every user's new files (the same reader,
  each file decoded once), saves the caches and exits, so the models' memory is gone until the next run.
  - The server starts it at `"index_at"` (local time, default `"03:00"`) and copies its output into its own log, so
    the `reading:` progress lines show as before.
  - It can also be run by hand at any time; it then tells the server through the admin API (`indexesUpdated`).
  - Ctrl+C, on it or on the server, finishes the file in hand.
- **The server keeps:**
  - SigLIP's text half, at full precision (the same model the photos' vectors were made with, so search finds
    exactly what it did), **loaded only while searches come**. The first search after a quiet spell loads it, about
    4 s (3.5–4.7 s measured, the file in the system's cache; longer after a restart of the PC). It is released 10
    minutes after the last search (`search.IdleRelease`). Searches meanwhile answer as before (60–90 ms for the
    text).
    - **Apps load it ahead:** `prepareSearch` (sent when the user opens the search screen) starts the load in the
      background and answers at once: `LOADING`, or `READY` when it was loaded already. Apps show that search is
      getting ready and ask `searchModel(wait: true)` once: it answers when the load ends (`READY`, or `UNLOADED`
      when it failed), at most 30 s, so apps don't ask again and again. A search sent meanwhile waits for the same
      load. `prepareSearch` keeps the model 10 minutes longer, as a search does. A server in `"server"`
      mode always answers `READY`, and one without text search `OFF`.
  - the caches, opened **read-only**, never cut or written. It reads what a run added when the run ends, when told
    (`indexesUpdated`), and every 10 minutes as a safety net. Faces then group the new faces, and similar finds the
    new duplicates in the background.
  - No reading model is loaded and there is no reader.
- **What waits for the night:** new uploads are found by words, faces, text or likeness only after that day's run.
  They aren't hidden for nudity until then either: the nudity scan runs with the others (the owner's choice, Oct 23,
  2026).
- **Memory, measured** (Oct 23, 2026, with the owner's caches: 295 MB of search vectors and 32 MB of faces):
  - **3.05 GB** with `"server"`;
  - with `"daily"`: **about 0.4 GB idle**, 1.6 GB while searches come, about 0.5 GB again 10 minutes after the last.
  - SigLIP's text half alone is 1.15 GB, whatever ONNX Runtime's settings (no arena, no prepacking or basic
    optimization made no difference), hence loading it only to search.
  - The caches stay in memory, to answer at once. They are read with room for every record at once, and what that
    leaves behind is given back to the system right away.
- **One writer at a time:** whoever writes the caches holds `.cache/index.lock` in each data folder. A server in
  `"server"` mode holds it while its own reader runs, and `journal-server index` holds it for its run. A second run,
  or a run beside a `"server"` server, refuses at once.

## Text in photos (OCR)
- **Models:** Rivok/paddleocr-hebrew (Apache 2.0), PaddleOCR finetuned for Hebrew: the word detector (DBNet,
  4.6 MB) and the PP-OCRv6 word recognizer (60 MB). The official PP-OCRv5 has no Hebrew. The recognizer's 120
  characters are Hebrew, Latin letters, digits and signs, so one model reads mixed lines.
  - Its card reports 2.2% character errors on Hebrew words and 2.0% on Latin.
  - The small "mobile" recognizer (7 MB) was 17× faster here, but dropped and swapped letters on rendered Hebrew
    and English.
- **Steps:**
  1. The detector reads the photo fitted in 1280 px: blue, green and red, ImageNet's normalization, padded to a
     multiple of 32.
  2. Its text map is cut at 0.3. Each blob whose mean reaches 0.35 becomes a word, grown by 1.5 × area / perimeter
     (1.3 cut off final letters).
  3. Words under 10 px high are dropped (specks, icons), and at most the 200 biggest of a photo are read.
  4. Each word is cut from the original at 48 px high (up to 320 wide) and read one at a time; 16 at once was
     slower on a CPU. The reading is CTC: the likeliest character at each step, repeats and blanks dropped.
  5. Words read less surely than 0.5 are dropped.
  6. Words become lines by their overlap, in reading order: right to left when a line is mostly Hebrew. Hebrew is
     stored in logical order, as it is typed.
- **Search:** `searchMedia(text:)` puts photos whose own text has every word first, with score 1, then SigLIP's
  matches. A word matches:
  - the same word, ignoring niqqud, final letters' shapes and case;
  - a word it begins, when it has 3 letters or more;
  - in Hebrew, the word with up to two prefix letters: ה ו ב כ ל מ ש ("קבלה" finds "בקבלה").
- **API:** `Media.text` gives the lines (text, box, score).

## Similar photos and duplicates (DINOv2)
- **Model:** DINOv2 small (Meta, Apache 2.0; onnx-community's export), 88 MB. It compares how photos look rather
  than what they show.
- **Reads** the original's centre at 224×224. The vector is its class token, scaled to length 1.
- **`similarMedia(sha256:)`:** the files most like one, best first.
- **`duplicates`:** groups of copies.
  - Two files are copies at a cosine of **0.96** or more, and copies that link through each other are one group.
  - Measured on the owner's photos: a photo and a smaller, recompressed copy score 0.974–0.993; the same scene shot
    again seconds later 0.90–0.95 (text search's sets already keep those together); different photos 0.07–0.86.
  - The first time, every pair is compared, on all the CPUs, after the reader's run; then only new files.
  - A group lists the best-looking copy first (aesthetic score), else the first uploaded.

## Aesthetic scores
- **Model:** LAION's aesthetic predictor ("sac+logos+ava1-l14-linearMSE", christophschuhmann/
  improved-aesthetic-predictor, Apache 2.0), on OpenAI's CLIP ViT-L/14 (MIT; Xenova's 8-bit export, 300 MB).
  - LAION's head has five linear layers with no activation between them, so `bundle-onnx` multiplies them into one
    (768 weights and a bias, `laion-head.json`). The server needs no PyTorch.
  - Aesthetic Predictor V2.5 was left out: it is AGPL.
- **Reads** the original's centre at 224×224, exactly as CLIP was trained (the short side to 224, the centre cut).
  The score is the head applied to CLIP's image vector scaled to length 1.
- **Scale:** about 1 to 10. The owner's sample photos scored 4.2–5.3 (median 4.55), random noise 3.1, flat grey
  4.4. Around 6 and above is a good photo.
- **API:** `Media.aesthetic`. `duplicates` uses it to put the best copy first.

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
