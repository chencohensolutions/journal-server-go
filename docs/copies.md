# Copies of the data folders

The server can keep a plain copy of each user's data folder in other folders: another disk, a USB disk or a
network share. A copy holds the originals, their sidecars, `people.json`, `settings.json`, `cameras.json` and the relabel journals (`relabels/`), in the data folder's
own layout (`media/<year>/<year-month>/…`). It can be read without the server, and in an emergency a server's
`data_folder` can point at it (the indexes are then rebuilt, since `.cache/` isn't copied).

Copies are optional and set per user. Copies to other machines over the internet, encryption, history and the
apps' view of copies are not built yet.

## Setting it up

In config.json, each user can list copies:

```json
{ "username": "hen", "data_folder": "D:\\Journal\\hen",
  "copies": [
    { "folder": "E:\\Journal copy\\hen", "realtime": true },
    { "folder": "F:\\hen", "at": "04:00" }
  ] }
```

- `folder` must exist: create it once. An unplugged disk is then noticed and reported, instead of a new folder being
  made where the disk was.
- `at` is when the daily full pass runs, in local time (default `"03:30"`).
- `realtime: true` also copies each file a few seconds after it arrives or its sidecar changes. That covers uploads,
  imports, scans, and marks such as private, whether made by the server or by a script. It also makes a full pass
  when the server starts, for what arrived while it was stopped.
- The server refuses a copy folder inside the data folder, or one that holds it. At start it warns about a copy on
  the data folder's disk, since a failing disk would take both.

## What is copied, and how

The copy folder itself is the record of what was copied; there is no other state.

- **Originals.** The file list comes from the server's index. An original is in the copy when a file of the same size
  is at its path, since originals never change. A missing one is copied to a temp file beside its place
  (`.<name>.*.copying`), checked against its sha256, and only then renamed into place. A copy therefore never holds
  part of an original. Before each original, the copy's disk must have the file's size plus 1 GB free; if not, the
  pass stops ("the copy's disk is full").
- **Sidecars, people.json, settings.json.** One of these is copied when its size differs from the copy's, or its
  modified time and its bytes both differ. The copy gets the source's modified time, so the next pass can compare
  them without reading them. The time is only a hint that a file changed, never a photo's date.
- **Nothing is deleted from a copy.** A file gone from the data folder stays in the copy.
- **The data folder is only read.** Copying writes no files, locks or journal lines there, and takes no turn.
  Sidecars are always rewritten whole (by a rename), so reading one without a turn gives a whole file.
- A file that fails is logged and counted, and the pass goes on. A missing folder or a full disk stops the pass.
  Realtime copies that fail wait for the next full pass.

Realtime copying uses `Store.OnWrite`, which the store calls after each of its turns with the files it placed and
the sidecars it rewrote. It also calls it as the store catches up with other processes' writes from the write
journal. One realtime batch, or one pass, runs at a time per copy.

## Commands

Both are admin commands, so the server must be running.

```bash
journal-server copy [user]                 # start a full pass of the user's copies now (all users' without one)
journal-server copy report [user] [--list] # how each copy is doing, and what it lacks now
```

The report compares the data folder with the copy without copying anything:

```
hen → E:\Journal copy\hen   (realtime, daily 03:30)
  last pass:    Oct 23 03:30, done in 3m12s, 120 files (1.4 GB), 0 failed
  not copied:   37 originals (412.0 MB), 5 sidecars
  in copy only: 2 originals (kept; gone from the data folder)
```

"Last pass" covers the passes since the server started. `--list` prints the paths of the files the copy lacks.
Every pass also writes one line to the server's log.

## Code

- `copies/`: `Copier` (`Run` a full pass, `Check` the report's compare, `Changed` and `RunRealtime` for realtime
  copies), and `disk_*.go` for the same-disk check per OS.
- `copy.go`: making the copiers from config.json, the daily schedule, and the realtime start.
- `auth.CopyConfig`, and `checkCopies` in `auth/config.go`.
- The admin schema's `copyReport` and `startCopy`, and `admincli.go`'s `copy` commands.
