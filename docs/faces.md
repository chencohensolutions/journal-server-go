# Faces

Devices find photos by the people in them. The server finds the faces in each photo and puts alike faces in groups.
The user names a group once ("Mom"), and from then on her faces are found as photos come in. It is done on the
server's own CPU with two small open models. Nothing leaves the server: the models are downloaded once, and photos are
never sent anywhere.

## The models

Both models are from OpenCV's model zoo, as ONNX files, and are run by the ONNX Runtime that text search uses
(`onnx/`; see [search.md](search.md)). Their licences allow use in a server handed to others. InsightFace's
better-known models were left out because their weights are for research only.

| | YuNet (2023mar) | SFace (2021dec) |
|---|---|---|
| Does | finds faces: a box, how sure it is, five points (eyes, nose tip, mouth corners) | turns a face into 128 numbers |
| Licence | MIT | Apache 2.0 |
| Size | 0.2 MB | 37 MB |
| Reads | 640×640, blue-green-red, 0–255 | 112×112, red-green-blue, 0–255, aligned on the five points |

Two faces of the same person point the same way. OpenCV takes a cosine of 0.363 or more between two faces as the
same person.

## How it works

- **Reading files.** In the background the server reads every photo and video it hasn't read yet, newest upload
  first. An upload is read as soon as it finishes, and an import within 30 seconds.
  - Each file is read from a 1280 px JPEG preview, `media.FacePreview`, bigger than search's 640 px so that faces at
    the back of a group are still clear. A video is read from its poster frame.
  - YuNet takes 640×640 only. So the whole picture is searched shrunk to 640, for the big faces. It is then searched
    again in overlapping 640 px tiles at full size (up to 3 × 3), for the small ones.
  - A face cut by a tile's inner edge is left to the tile beside it or to the whole picture. Boxes that overlap by
    more than 0.3 are one face, and the surest is kept.
  - Faces the detector is less than 0.6 sure of are dropped, and so are faces narrower than 16 px.
  - Each face is turned and scaled so its five points sit on SFace's template (`faces/align.go`, plain Go), then read
    into a vector of length 1.
  - It takes about 0.15 s for a photo of a few people on the owner's PC with 4 threads, plus the preview. A crowd of
    a hundred faces takes 1.3 s, because each face is read on its own (Oct 23, 2026). A file that can't be read is
    logged and tried again after a restart. It is also listed in `.cache/faces/<model>.failed.log`, one tab-separated
    line each: when (UTC), sha256, path in the data folder, and why. Every failure is listed, including those not
    logged one by one: without libvips or ffmpeg every file or video fails alike, and the console only says so once at
    start. The list is appended to on each run, so a file that keeps failing appears once per restart. It can be
    deleted. Text search keeps the same list (`media.FailureLog`).
- **Groups.** Faces no one is named for are put together, in the order taken, so that a day's photos of someone
  meet (`faces/people.go`).
  - A face joins the first group whose mean it is at least 0.45 like; otherwise it starts a group.
  - A group of one is only compared with the last 2000 such groups, which keeps this fast. 10,000 photos take about
    half a second.
  - Only clear faces are grouped: the detector at least 0.8 sure, and at least 40 px wide in the preview.
  - Groups of fewer than 3 faces aren't shown: most are strangers in the background.
  - Groups are found again whenever the user changes a person, so a group's id (`g…`) can change. A person's id
    (`p…`) never does.
- **People.**
  - `namePerson` on a group makes a person, and the group's faces are theirs. A group given an existing person's name
    (ignoring case) joins that person, and naming a person renames them.
  - From then on, a clear face is the person's when it is at least 0.45 like the mean of the faces the user named.
    It also counts when it is at least 0.3 like that mean and at least 0.5 like one of 64 of those faces: a child
    grows, a face is seen from the side.
  - `moveFace` takes a face from a person, who is never given it again, and gives it to another person or to no one.
  - `mergePeople` puts people and groups into one person. `forgetPerson` removes a person, and their faces go back
    into groups.
  - The thresholds are first guesses from OpenCV's figures. They are meant to be tuned on the owner's photos.
- **What a device sees.** Everything goes through the device's view, as for every other read:
  - Faces in hidden collections are never counted.
  - Faces in files the user marked private count only with `includePrivate: true` (an app showing private media).
  - On a device set to withhold private files, they never count at all.
  - A person or group left with no face the device may see is left out. A group left with fewer than 3 is too.

## The API

- `people(includeGroups, includePrivate)`: named people first, by name, then groups, those in the most files first.
  Each has an `id`, a `name` (null for a group), a `cover` face (the clearest), and how many `faces` and `files`.
  `read` of `total` says how far the server has got.
- `Media.faces`: the faces in a file, biggest first, each with its `box` (fractions of the upright photo, the same as a
  preview's `crop`) and the `personId` / `personName` it is in. An app draws a face from the file's preview with
  `crop=x,y,w,h`; nothing new is needed on the server for that.
- `searchMedia(people: [id…])`: only files with a face of each of them. With text too, the best matches of the text
  among them; without text, those files, the last taken first. `years`, `area` and private files work as in a text
  search.
- `similarFaces(face: id)`: "more photos of this face". It returns the files with a face most like it, best first, one
  face a file, leaving out the face's own file. A score of 0.36 or more is likely the same person.
- `namePerson`, `mergePeople` and `moveFace` answer `Person` / `Face`, or `NotFound` when an id is stale (the app's
  list is older than the server's). The app then loads the people again. `forgetPerson` answers whether there was
  such a person.

## Where things are

- `faces/`:
  - `model.go` loads the two models and reads a picture into faces.
  - `detect.go` holds YuNet, the tiles and the overlap rule.
  - `align.go` turns and scales a face onto the template.
  - `index.go` is the faces file.
  - `people.go` assigns faces to people and puts the rest in groups.
  - `api.go` answers the queries and makes the changes.
  - `service.go` reads new files.
- **Faces** are kept per user and per model in `.cache/faces/<model>.faces`.
  - The file is a header, then one record per file read, appended. A record is the sha256's 32 bytes, a 2-byte count,
    and per face its box, score, five points and vector, as little-endian float32s.
  - A file without faces has a record too, with a count of 0, so it isn't read again.
  - A record cut short by a stop is dropped at the next start.
  - The file can be deleted: the server reads everything again. Only the server writes it.
- **The user's choices** are in `people.json` at the data folder's root, so they move with the data:
  - each person's id, name, the faces they are (`faces`), the faces they are not (`notFaces`), and when the person was
    last changed;
  - a face is named by its file's sha256 and its box rather than by its place in the faces file, so the choices
    outlive a change of model (the face found closest to the box, overlapping it by half or more, is taken);
  - it is written whole inside a turn.

## Setting it up

```bash
go run ./scripts/bundle-onnx    # ONNX Runtime, the search model and the face models (in models/yunet-sface)
```

`--faces=false` leaves the face models out. The server turns face search on at start when the folder exists;
otherwise it logs why it is off, `people` answers an error and `Media.faces` is empty. Settings in `config.json`:

| Setting | Default | |
|---|---|---|
| `face_model` | `models/yunet-sface` | the folder of the two models |
| `onnxruntime` | as for search | the ONNX Runtime library |
| `search_threads` | half the CPUs | threads each model may use |

Tests that need the models skip without them. Those that need photos of faces skip unless `JOURNAL_FACE_TESTDATA`
names a folder of JPEGs, since none are kept in the repository:

```bash
JOURNAL_FACE_TESTDATA=~/faces go test ./faces . -run Face
```

Not built yet: the apps' side (a People section in the search filter, a people strip, "more of this face" in the
viewer), and tuning the thresholds on the owner's library.
