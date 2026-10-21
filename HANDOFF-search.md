# Handoff: turn on search on the Windows server

For a Claude Code session on the server machine (Windows, 16 GB RAM, no GPU). The owner wants search by what
photos show running there. The code is done and tested on the owner's Mac. What's left is setting up this machine.
Delete this file once search runs.

## What was built (Oct 21, 2026)

- Package `search`: the server keeps a Python worker running (`search/siglip_worker.py`, embedded in the binary)
  with the SigLIP 2 base model (`google/siglip2-base-patch16-384`, Apache 2.0).
  - It reads every photo once in the background, newest first, from its 640 px preview (libvips), and a video
    from its poster (ffmpeg).
  - Vectors go in `<data_folder>/.cache/search/<model>.vectors`, a cache that can be rebuilt by deleting it.
- GraphQL: `searchMedia(query, limit)` returns matches with scores, plus `indexed` and `total`.
- It is **off unless `config.json` has `"search"`**. Without it, or without Python, the server runs as before.
- Full details: `docs/media.md`, section "Search". The code map is in `CLAUDE.md`.
- Checked on the Mac: `go test ./...` passes, and the real model ranked sample pictures correctly.
  - English queries were good.
  - Hebrew was mixed: "פינגווין" (penguin) and "פרח אדום" (red flower) worked; "משהו מהים" (something from the
    sea) missed.
  - The owner may later try `google/siglip2-so400m-patch16-384`, which is better at Hebrew but about 4.5 GB.
    Don't switch without asking.

## Steps

1. **Get the code:** `git pull`. Then check that it builds and that the search tests pass. These tests need no
   Python: they use a stand-in model.
   ```powershell
   go build -o journal-server.exe .
   go test ./search/... .
   ```
2. **Python and the libraries**, if they're not installed yet. Ask the owner before installing anything.
   ```powershell
   winget install Python.Python.3.12
   # new terminal, so python is on the PATH
   python -m pip install torch --index-url https://download.pytorch.org/whl/cpu
   python -m pip install transformers pillow
   python -c "import torch, transformers, PIL; print(torch.__version__, transformers.__version__)"
   ```
   - The CPU-only build of torch is about 200 MB; the default build bundles CUDA (about 2.5 GB) for a GPU this
     machine doesn't have.
   - transformers must be 4.49 or newer.
   - If `python` opens the Microsoft Store, Windows' built-in app shortcut is in the way. Use the full path to
     `python.exe` in step 4.
3. **Check the worker by itself.** The first run downloads the model, about 1.5 GB, into
   `%USERPROFILE%\.cache\huggingface`, which takes minutes.
   ```powershell
   '{"text": "a dog"}' | python search\siglip_worker.py google/siglip2-base-patch16-384
   ```
   - Expect two JSON lines: `{"ready": true, "dims": 768, "device": "cpu"}`, then `{"vector": "..."}`.
   - Anything else (a Python traceback, or `{"error": ...}`) means the setup is incomplete. Fix that before going on.
4. **Turn it on in `config.json`:** add `"search": {}` at the top level.
   - The defaults are the base model and `python` on Windows.
   - Set `"python": "C:\\...\\python.exe"` only if `python` isn't on the PATH the server runs with.
   - `config.json` is committed and holds the owner's real config. Change only this key, and don't commit it
     without asking.
5. **Video posters (optional):** without ffmpeg, videos are skipped (photos still work).
   - `winget install Gyan.FFmpeg`, or set `"ffmpeg"` in `config.json`.
   - libvips must already be there for previews (`vips/` from `go run ./scripts/bundle-vips`). Check that it is.
6. **Restart the server** the way the owner normally runs it. Ask; don't guess. Then watch the log for:
   ```
   search: loading google/siglip2-base-patch16-384 (the first start downloads it)
   search: google/siglip2-base-patch16-384 loaded (768 dimensions, on cpu)
   search: <user>: reading N files
   search: <user>: 1200 of N read (6.3/s)
   ```
   - The first pass takes hours on CPU (roughly 5–15 photos a second). It continues where it stopped after a
     restart, and the server serves meanwhile.
   - If you see `search: ... trying again in 1m0s`, the worker failed to start: go back to step 3.
   - `N files couldn't be read, such as ...` is normal for videos without ffmpeg.
7. **Try a search** from an enrolled device, or the playground at `https://<server>:56398/`, which needs a client
   certificate:
   ```graphql
   { searchMedia(query: "dog on the beach") { indexed total hits { score media { fileName } } } }
   ```
   `"search is starting"` means the model is still loading; wait and try again.

## Report back to the owner

- The torch and transformers versions, and whether ffmpeg is installed.
- How fast the first pass runs (the `/s` in the log), and the memory of the Python process (Task Manager).
- A few real searches, in English and in Hebrew, and whether the results look right.
