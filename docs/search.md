# Text search

Devices find photos and videos by describing them in words: "a dog on the beach at sunset", "birthday cake",
"receipt". The server does it with **SigLIP 2**, Google's image and text model, run on its own CPU. Nothing leaves
the server: the model is downloaded once, and photos are never sent anywhere.

## How it works

SigLIP 2 has two halves trained together. The **vision half** turns a picture into a vector (768 numbers for the
base model), and the **text half** turns a description into a vector in the same space. The better a description
fits a picture, the closer their vectors point; the closeness is their cosine similarity.

- **Reading files.** In the background, the server reads every photo and video that has no vector yet, newest upload
  first: it makes the 640 px JPEG preview, `media.ModelPreview` (with libvips; a video's poster frame with ffmpeg; the same preview
  `scripts/scan-nudity` reads), squeezes it to 256×256 and runs the vision half. An upload is read as soon as it
  finishes, an import within 30 seconds. A file that can't be read is logged and tried again after the server restarts.
- **Searching.** `searchMedia(text, limit)` lowercases the text, turns it into 64 tokens and runs the text half
  (about 50 ms), then compares it with every vector in memory (a few milliseconds for tens of thousands of files)
  and returns the closest, best first. With `years` and `area` (a box of latitudes and longitudes: the server has no
  place names, so the app sends the box around a town's photos) only files of those years, or taken inside the
  box, are considered, before the best are picked; a filtered search returns the best of that year or place, not the
  overall best that happen to match. A file's year is where it was taken (its time's offset), or the year it was
  uploaded when that isn't known, as the apps date it; files without a location never match an area. Files the device may not see (private files on a device set to withhold
  them) are left out. The answer also says how many files are read so far (`indexed` of `total`), so an app can say
  that search is still catching up.
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

- `search/` is the package. `ort.go` loads ONNX Runtime and calls it through its C function table with purego (no
  cgo; `ort_windows.go` / `ort_unix.go` load the library). `model.go` prepares pictures and text and runs the two
  halves. `index.go` is the vector file. `service.go` reads new files and answers searches.
- Vectors are kept per user and per model in the data folder: `.cache/search/<model>.vectors`. It is a header and
  then one record per file (its sha256's 32 bytes, then the vector as little-endian float32s), appended as files are
  read; a record cut short by a stop is dropped at the next start. It can be deleted: the server reads everything
  again. Only the server writes it (no turns needed: it is in `.cache` and no other process touches it).
- The model folder holds `vision_model.onnx`, `text_model.onnx` (plus `*.onnx_data` for the large model's weights),
  `tokenizer.model` (Gemma's SentencePiece vocabulary, read by github.com/eliben/go-sentencepiece) and
  `preprocessor_config.json` (picture size and scaling).

## Setting it up

```bash
go run ./scripts/bundle-onnx                  # ONNX Runtime in ./onnxruntime, the base model in ./models
go run ./scripts/bundle-onnx --model large    # the larger model (see below)
```

Both folders are gitignored. The server turns search on at start when the model folder exists; otherwise it logs
why search is off and `searchMedia` answers an error. Settings in `config.json`:

| Setting | Default | |
|---|---|---|
| `search_model` | `models/siglip2-base-patch16-256` | the model folder |
| `onnxruntime` | `./onnxruntime`, then Homebrew's (`brew install onnxruntime`) | the ONNX Runtime library |
| `search_threads` | half the CPUs | threads each half of the model may use |

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

Not built yet: finding photos like a given one, date and place filters, and captions (a model on another computer
describing each photo).
