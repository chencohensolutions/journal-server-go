# Handoff: turn on text search on the server

For a Claude Code session on the server machine (Windows, 16 GB RAM, no GPU). The owner wants text search
(`searchMedia`, "a dog on the beach at sunset") running there. The code is done. It was tested end to end on the
owner's Mac on Oct 22, 2026, with the Android emulator and the iOS simulator searching a local test server. What's
left is setting up this machine. Delete this file once search runs here.

**This replaces the earlier version of this file.** That version described a Python worker (torch, transformers,
`"search": {}` in config.json). Commit 168f5e0 removed it. Search now runs inside the server with ONNX Runtime,
loaded through purego (no cgo), so nothing Python is needed. If Python and torch were installed for the old plan, they
can stay or go; the server doesn't use them.

## How it works (short)

- `search/` is the package; read [docs/search.md](docs/search.md) for the details.
- At start the server loads ONNX Runtime and a SigLIP 2 model folder. Then it reads every photo once in the
  background, newest upload first, from its 640 px preview (libvips). A video is read from its poster frame, which
  needs ffmpeg.
- Vectors are kept in `<data_folder>/.cache/search/<model>.vectors`. It's a cache: deleting it makes the server read
  everything again.
- Without the runtime or the model, the server runs as before. It logs `Search is off (...)`, and `searchMedia`
  answers an error.
- The apps (Android and iOS, not released yet) have a 🔍 beside the tabs that calls `searchMedia`. They show
  "Still reading your photos: X of Y" while the first pass runs.

## Steps

1. **Get the code and check it builds.** The search tests skip what needs the model, so they pass before step 2.
   ```powershell
   git pull
   go build -o journal-server.exe .
   go test ./search/... .
   ```

2. **Download ONNX Runtime and the model.** This is about 1.5 GB and takes minutes. `scripts/bundle-onnx` picks the
   Windows x64 (or arm64) build of ONNX Runtime by itself.
   ```powershell
   go run ./scripts/bundle-onnx
   ```
   - It creates `onnxruntime\` (the DLLs) and `models\siglip2-base-patch16-256\` (`vision_model.onnx`,
     `text_model.onnx`, `tokenizer.model`, `preprocessor_config.json`). Both folders are gitignored.
   - It ends with `Done. In config.json: "search_model": "models/siglip2-base-patch16-256"`.
   - Use the base model (the default). `--model large` (about 4.5 GB, better at Hebrew, about 4× slower) and
     `--precision q8` exist. Don't switch to either without asking the owner.

3. **`config.json`: most likely nothing to change.** These are the defaults:

   | Setting | Default |
   |---|---|
   | `search_model` | `models/siglip2-base-patch16-256` |
   | `onnxruntime` | the library in `./onnxruntime` |
   | `search_threads` | half the CPUs |

   - Set them only if the files live elsewhere, or if the owner wants to limit CPU use.
   - Paths are relative to the folder the server runs in. If the server is started from another folder, use
     absolute paths (`"C:\\..."`).
   - `config.json` is committed and holds the owner's real config. Change only these keys, and don't commit it
     without asking.

4. **Check the image tools.**
   - **libvips** must already be in `vips\` (from `go run ./scripts/bundle-vips`), or the server can't make the
     previews search reads. Check that the server's log says `Previews made with ...`.
   - **ffmpeg** is needed for videos only. Without it, videos are skipped and photos still work. Install it with
     `winget install Gyan.FFmpeg` (ask the owner first), or set `"ffmpeg"` in config.json.

5. **Restart the server the way the owner normally runs it.** Ask; don't guess (a scheduled task, a service, or a
   terminal). Then watch the log for:
   ```
   Text search with siglip2-base-patch16-256 on ONNX Runtime 1.x (N threads), loaded in 2.5s
   search: reading 18000 files of <user> with siglip2-base-patch16-256
   search: read 49 files of <user> in 14s; 0 could not be read
   ```
   - `Search is off (...)` means the model folder or the runtime wasn't found. Check the paths from step 3, and that
     `onnxruntime\onnxruntime.dll` exists.
   - The first pass runs at about 0.18 s per photo on an i7-10700 with 4 threads, roughly 5 photos a second, so
     several hours for a large library.
   - The server serves devices meanwhile. After a restart, it continues where it stopped.
   - Files that can't be read (videos without ffmpeg, damaged files) are logged and tried again after the next
     restart.

6. **Try a search.** Use an enrolled device with a new app build, or the playground at `https://<server>:56398/`,
   which needs a client certificate:
   ```graphql
   { searchMedia(text: "a dog on the beach", limit: 10) { indexed total model hits { score media { fileName takenAt } } } }
   ```
   - Scores are small (a good match is often 0.1 to 0.2). Only the order means much.
   - Results so far: English works well. With Hebrew, "פרח אדום" (red flower) and "סופת ברקים" (lightning storm)
     worked; "משהו מהים" (something from the sea) didn't.

## Report back to the owner

- Whether search loaded, and the log line with the ONNX Runtime version and thread count.
- How fast the first pass runs (from the `read N files ... in Ns` lines), and the server's memory in Task Manager.
  The model takes roughly 1 to 1.5 GB.
- Whether ffmpeg is installed, and how many files couldn't be read.
- A few real searches, in English and in Hebrew, and whether the results look right.

## Open question for the owner (from the Mac session)

The server always returns the closest matches up to `limit` (the apps ask for 100), even when few really match. The
apps show them best first. The owner hasn't decided yet whether to hide weak matches (a score cutoff). Real results
from this library will help decide, so note what the scores of good and bad matches look like.
