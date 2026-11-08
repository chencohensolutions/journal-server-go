# Text search

Devices find photos and videos by describing them in words: "a dog on the beach at sunset", "birthday cake",
"receipt". The server does it with **SigLIP 2**, Google's image and text model, run on its own CPU. Nothing leaves
the server: the model is downloaded once, and photos are never sent anywhere.

## How it works

SigLIP 2 has two halves trained together. The **vision half** turns a picture into a vector (768 numbers for the
base model), and the **text half** turns a description into a vector in the same space. The better a description
fits a picture, the closer their vectors point; the closeness is their cosine similarity.

- **Reading files.** The server's reader ([indexes.md](indexes.md)) reads every photo and video that has no vector
  yet, newest upload first, decoding each original once for the nudity scan, faces and search together. Search gets
  the original squeezed straight to the model's square (256×256, read from `preprocessor_config.json`), as SigLIP was
  trained, and runs the vision half. A video is read from its poster frame. An upload is read as soon as it
  finishes, an import within 30 seconds. A file that can't be read is logged, tried again after the server restarts,
  and listed in `.cache/search/<model>.failed.log`.
  Vectors read before Oct 23, 2026 came from a 640 px preview squeezed in Go; they are kept, since the difference is
  small, and only new files get the new input.
- **Searching.** `searchMedia(text, limit)` lowercases the text, turns it into 64 tokens and runs the text half
  (about 50 ms), then compares it with every vector in memory (a few milliseconds for tens of thousands of files)
  and returns the closest, best first. With `years` and `area` (a box of latitudes and longitudes: the server has no
  place names, so the app sends the box around a town's photos) only files of those years, or taken inside the
  box, are considered, before the best are picked; a filtered search returns the best of that year or place, not the
  overall best that happen to match. With `people` (ids from the `people` query), only files with a face of each of
  them; `text` may then be left out, and the hits are those files, the last taken first ([faces.md](faces.md)). A file's year is where it was taken (its time's offset), or the year it was
  uploaded when that isn't known, as the apps date it; files without a location never match an area. Files the device may not see (private files on a device set to withhold
  them) are left out. Files the user marked private are left out on every device too, unless the app asks with
  `includePrivate: true` (it does while it shows private media); a device set to withhold them never finds them. The answer also says how many files are read so far (`indexed` of `total`), so an app can say
  that search is still catching up.
- **Getting ready.** `prepareSearch`, sent when the user opens the search screen, gets the model ready before the
  first search, and `searchModel` says whether it is (`READY`, `LOADING`, `UNLOADED`, `OFF`); with `wait: true` it
  answers when a load in progress ends (at most 30 s). A server that indexes
  once a day loads the text half only to search, about 4 s ([indexes.md](indexes.md)); one that reads files itself
  is always `READY`.
- **Pages.** `offset` skips the first hits for the next page (offset + limit at most 10000). The hits before it are
  still found, so a page's sets and moments carry on the numbers of the pages before it (a set split between pages
  has one number).
- **Sets and moments.** Each hit says which `set` and `moment` it is in (`search.Groups`, `search/group.go`).
  Almost identical photos (a burst, a pose taken again) are one set: a hit joins the first set whose best photo is
  at least 0.93 alike (cosine of the vectors) and taken within 10 minutes. Sets taken within 2 hours of a moment's
  first set share that moment. Both are numbered in the order of their best hit, so the apps keep each moment's sets
  together and show a set as one photo you swipe. The thresholds were measured on the owner's photos: frames of a
  burst score 0.93 to 0.99, a reframed shot seconds later 0.83 to 0.87, and the same table half an hour later still
  0.95, which is why time counts too. Files without a time are a set and moment of their own.
- **Scores** are SigLIP's cosine similarities, which are small: a good match is often 0.1 to 0.2. Only the order
  means much.

## Where things are

- `search/` is the package. ONNX Runtime is in `onnx/` (shared with face search, [faces.md](faces.md)): `ort.go`
  loads it and calls it through its C function table with purego (no cgo; `ort_windows.go` / `ort_unix.go` load the
  library). `model.go` prepares pictures and text and runs the two halves. `index.go` is the vector file. `service.go` reads new files and answers searches.
- Vectors are kept per user and per model in the data folder: `.cache/search/<model>.vectors`. It is a header and
  then one record per file (its sha256's 32 bytes, then the vector as little-endian float32s), appended as files are
  read; a record cut short by a stop is dropped at the next start. It can be deleted: the server reads everything
  again. Only the server writes it (no turns needed: it is in `.cache` and no other process touches it).
- The model folder holds `vision_model.onnx`, `text_model.onnx` (plus `*.onnx_data` for the large model's weights),
  `tokenizer.model` (Gemma's SentencePiece vocabulary, read by github.com/eliben/go-sentencepiece) and
  `preprocessor_config.json` (picture size and scaling). A full-precision folder also has `text_model_q8.onnx`, the
  8-bit text half, for phones only (below); the server never loads it.

## Setting it up

```bash
go run ./scripts/bundle-onnx                  # ONNX Runtime in ./onnxruntime, the base model in ./models
go run ./scripts/bundle-onnx --model large    # the larger model (see below)
```

Both folders are gitignored. The server turns search on at start when the model folder exists; otherwise it logs
why search is off, `serverInfo.search` is false and `searchMedia` answers an error with code `SEARCH_OFF`. Settings in `config.json`:

| Setting | Default | |
|---|---|---|
| `search_model` | `models/siglip2-base-patch16-256` | the model folder |
| `onnxruntime` | `./onnxruntime`, then Homebrew's (`brew install onnxruntime`) | the ONNX Runtime library |
| `search_threads` | half the CPUs | threads each half of the model (and each face model) may use |

## Models

| | base (default) | large |
|---|---|---|
| Hugging Face | google/siglip2-base-patch16-256 | google/siglip2-so400m-patch16-256 |
| Download | 1.5 GB | about 4.5 GB |
| Vector | 768 numbers | 1152 numbers |
| A photo, i7-10700, 4 threads | 0.18 s | about 4× longer |
| A search's text | 0.05 s | longer, still well under a second |

`--precision q8` downloads 8-bit versions of either: a quarter of the size and faster, slightly less exact. Each
model (and precision) is its own folder and keeps its own vectors, so switching `search_model` reads every file
again with the new one and leaves the old vectors in place for switching back.

Languages: SigLIP 2 was trained on many languages, but English works best. On a test set with the base model,
Hebrew "פרח אדום" (red flower) and "סופת ברקים" (lightning storm) found the right pictures, "משהו מהים" (something
from the sea) didn't; the large model is better at languages other than English.

## On the phone

A phone may keep any of the indexes and search them itself, each turned on in the app's settings: text search, faces
(people and similar faces), text in photos, similar photos and duplicates, and aesthetic scores. It then searches
without the server, offline too, and the server sends only what was added since the phone's last look, daily or
when the user asks. The package is `localindex/`. Everything below needs a device certificate, and only what the
device may see is listed or sent, as everywhere else.

- **The manifest.** `localIndexes` lists the indexes that are on: each one's `name`, `model`, record `format`,
  vector length (`dim`), how many of the device's `files` it has read, about how many `bytes` a download from the
  start is, and the `models` a phone needs to run it. Only text search needs one: the text half, to turn a query into a
  vector (`text_model.onnx`, 1.13 GB, the same as the server's; or `text_model_q8.onnx`, about 290 MB, the 8-bit
  one), and `tokenizer.model` for either. The photos' vectors are the server's, read with the full vision half;
  `TestPhoneTextModel` checks that the 8-bit text half's vectors are within 0.98 cosine of the full one's and find the
  same pictures first, so they rank the photos about the same. Faces, text in photos, similar photos and scores need no model on the phone: everything is already
  read. `serverInfo.localIndexes` says whether there is any.
- **The stream.** `GET /index/{name}?cursor=…` sends the records the index's cache file has after the cursor (none:
  all of them). It is a JSON line (`index`, `model`, `format`, `dim`, `reset`), then frames: a kind byte, the length of
  what follows as 4 bytes little-endian, and that many bytes.
  - `R` is a record, exactly as the cache file keeps it: `vectors` (the sha256's 32 bytes, then `dim` float32s),
    `faces` (the sha256, a 2-byte count, then per face 15 float32s of box, score and points, and `dim` of vector) or
    `jsonl` (the OCR line, without its newline). A file that comes again replaces what the phone had of it.
  - `D` is a sha256 (32 bytes) to drop.
  - `C` is a cursor to go on from should the stream break, every 4 MB of the file; `E` is the cursor for next time,
    and the end (a stream without it broke).
  - `reset: true` tells the phone to drop what it has of the index first: a new model, a cache file started over (the
    cursor names the last record it saw, which must still be there), or a cursor this server can't read.
  - Text in photos is gzipped when the phone accepts it; vectors hardly shrink, and aren't.
- **What the device may see.** Records of files the device may not see are left out. Before the new records, the
  stream sends what changed for files sent before: with another view than the cursor's (the user hid or showed a
  collection, the device was set to withhold private files), every file whose visibility differs, and every file whose
  sidecar changed since the last look (marked private or not, scored by the nudity scan). Each is dropped (`D`) or sent
  again from the index's memory (`R`), so hiding WhatsApp doesn't download the index again. Files deleted from the
  server aren't told here: the change feed tells the phone, which drops them from its indexes too.
- **People.** `GET /index/people` is the people and groups as `people` lists them (named first), each with every
  face of theirs in files the device may see, as face ids (`<sha256>:<n>`, n counting the file's faces in its record).
  Groups live only in the server's memory and are renumbered when the user names, merges or moves, so the document
  comes whole; its hash is the `ETag`, and `If-None-Match` answers 304 when nothing changed. Naming, merging and moving
  stay server mutations.
- **Model files.** `GET /models/{index}/{name}` sends a file the manifest lists, with Range (a gigabyte resumes where
  it broke) and its sha256 as the ETag. The server hashes them in the background at start and keeps the hashes in
  `.hashes.json` in the model folder; a file is listed once hashed.
- **Models a phone runs by itself.** `phoneModels` lists files a phone runs that aren't part of any index the server
  keeps, served the same way at `/models/phone/{name}`: today MediaPipe's photo embedder
  (`mobilenet_v3_large.tflite`, 11 MB, Apache 2.0), which the Android app finds near-duplicates with, so the app
  doesn't carry it. `scripts/bundle-onnx` and `journal-server setup` download it into `models/phone/` (version 1 from
  Google's MediaPipe model storage, pinned by its sha256); `"phone_models"` in config.json names another folder. A
  server without it lists none, and the app finds near-duplicates by dHash alone.
- **Downloading from where the models came from.** Each model file a phone may download (`phoneModels` and
  `localIndexes`' models) can carry a `source`: the link bundle-onnx downloaded it from, pinned so it doesn't
  change (Hugging Face at the repository's commit for SigLIP's text half and tokenizer, Google's MediaPipe storage at
  version 1 for the photo embedder). bundle-onnx writes them in `.sources.json` in each model folder, and the server
  offers a source only while the file's sha256 is still the one written there. The phone downloads from the source
  first, so 290 MB per phone doesn't go out through the server's home upload, checks it against the server's
  `sha256` (the phone's text half must be exactly the server's, or its searches won't match the index), and falls
  back to `url` here. The phone then contacts Hugging Face or Google once: they see its address and which model it
  took, nothing of the photos (the owner's choice, Nov 8, 2026). A model downloaded before then has no source until
  bundle-onnx runs again.
- **Doing what the server does.** A phone's search must give the server's answers. `go run ./scripts/local-fixtures
  --out <folder>` writes made-up cases with the server's answers: the tokens and vectors of queries (fp32 and 8-bit),
  text in photos' words, matches and order, grouping into sets and moments, and the thresholds. The apps test against
  them.

Not built yet: finding photos like a given one, date and place filters, and captions (a model on another computer
describing each photo).
