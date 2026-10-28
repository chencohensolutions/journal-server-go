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

- **Reading files.** The server's reader ([indexes.md](indexes.md)) reads every photo and video faces hasn't read
  yet, newest upload first, decoding each original once for the nudity scan, faces and search together. An upload is
  read as soon as it finishes, and an import within 30 seconds.
  - Faces gets two pictures, each made from the original: the photo fitted in **640** (YuNet's size) and in **1280**
    (`media.FacePreview`, big enough for faces at the back of a group). A video is read from its poster frame.
  - YuNet takes 640×640 only. So the whole photo is searched in the 640 picture, for the big faces. It is then
    searched again in overlapping 640 px tiles of the 1280 one (up to 3 × 3), for the small ones.
  - A face cut by a tile's inner edge is left to the tile beside it or to the whole picture. Boxes that overlap by
    more than 0.3 are one face, and the surest is kept.
  - Faces the detector is less than 0.6 sure of are dropped, and so are faces narrower than 16 px.
  - Each face is cut **from the original itself** (`Original.Region`): a square 2.5 times the face, at the scale
    where its eyes are as far apart as SFace's template wants, so a small face in the background keeps the
    original's detail instead of the 1280 picture's. It is then turned onto the template (`faces/align.go`, plain
    Go) and read into a vector of length 1.
  - It takes about 0.15 s for a photo of a few people on the owner's PC with 4 threads, plus the preview. A crowd of
    a hundred faces takes 1.3 s, because each face is read on its own (Oct 23, 2026). A file that can't be read is
    logged, tried again after a restart, and listed in `.cache/faces/<model>.failed.log` (see
    [indexes.md](indexes.md)).
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
  - A face the user said a person is not also keeps its lookalikes from them (Oct 24, 2026): a face at least 0.5
    (`NotMatch`) like a rejected face, and more like it than like any of the person's own faces (face to face), isn't
    theirs. So "Not Mom" on a sister's faces stops her other photos going to Mom, more with each "No" (tested with a
    sister about 0.8 alike: two "No"s took most of her photos away, and Mom kept all of hers).
  - `markFaces(person, yes, no)` makes many such choices in one change (each change puts every face together again,
    a few seconds for a large library), and `nameFace(face, name)` gives one face to a person by name, or to a new
    person of that name.
  - The people document a phone keeps (`GET /index/people`) carries each named person's choices (`named`, `not`), so
    a phone matches faces as the server does and shows a correction at once.
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
- A phone that keeps the faces ([search.md](search.md) "On the phone") gets the faces file's records as a stream, and
  the people and groups with their faces whole (`GET /index/people`). It then finds people and similar faces itself.
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
    outlive a change of model (the face found closest to the box, overlapping it by half or more, is taken); a face
    named in a JPEG since kept as JPEG XL keeps the JPEG's sha256 here, and is found in the JPEG XL, whose faces are
    the JPEG's (`people.current`, through `Store.Find`);
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
