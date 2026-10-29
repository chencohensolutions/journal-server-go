# Copies of the data folders

The server can keep a plain copy of each user's data folder in other folders: another disk, a USB disk or a
network share. A copy holds the originals, their sidecars, `people.json`, `settings.json`, `cameras.json` and the relabel journals (`relabels/`), in the data folder's
own layout (`media/<year>/<year-month>/…`). It can be read without the server, and in an emergency a server's
`data_folder` can point at it (the indexes are then rebuilt, since `.cache/` isn't copied).

Copies are optional and set per user. A phone sees how its user's copies are doing and can start a pass ("In the
apps", below). A copy can also be on a friend's journal server, a peer ([peers.md](peers.md)): it works as this
page says but for what peers.md says. Encrypted copies and restore are planned in
[../../journal-client-kmp/docs/plans/swap-and-encryption.md](../../journal-client-kmp/docs/plans/swap-and-encryption.md).

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
- **Nothing is deleted from a copy**, but for one thing. A file gone from the data folder stays in the copy. A JPEG
  the data folder keeps as JPEG XL ([media.md](media.md) "JPEG XL") leaves the copy too (the owner's choice, Oct 28,
  2026), and only when it really is the same file: once the copy has the JPEG XL, the copy's JPEG (found in the
  same month folder by its sidecar's sha256) must have the sha256 the JPEG XL's `encodedFrom` names, and the copy's
  JPEG XL, decoded with libjxl, must make it byte for byte. Then the JPEG's sidecar and the JPEG are deleted from
  the copy, and the pass counts it (`Dropped`, logged with the bytes freed). Without libjxl, or when either check
  fails, the JPEG stays and the pass counts it as failed, every pass, so it shows in the log.
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

## In the apps

Since Oct 28, 2026 the public schema has `copies` and `startCopy(folder:)` (`graph/schema/copies.graphql`;
`serverInfo.copies`), for the apps' Settings › Backup › Copies on other disks. A device sees only its own user's
copies, and only counts and the copy's folder, never file names; the counts include files hidden on devices.

- **The counts come from the last compare**, the report's compare kept per copy (`Copier.CompareNow`,
  `LastCompare`): how many originals the data folder had (`originals`), how many the copy held (`copied`), what it
  lacked (`missing`, `missingBytes`) and `inCopyOnly`, with `checkedAt`. Comparing reads every original's size in
  both folders, seconds on a USB disk, so a request never waits for one: the server compares after every full pass,
  and a request starts one in the background (`checking` until it ends) when the last is 10 minutes old, files were
  copied in real time since, or it failed. A compare that can't reach the folder keeps the last good counts.
- **They outlive a restart**: after each good compare and each full pass the copier writes both into
  `.journal-copy.json` at the copy folder's root (`copies/saved.go`, `"v": 1`), and reads it back when the server
  starts, so the apps show the last counts and pass at once instead of "comparing" for the minutes a first compare
  takes. A compare walks only `media/`, so the file is never counted as a file in the copy only.
- **Problems**: `FOLDER_MISSING` from the first time a pass, a compare or a realtime batch found the folder out of
  reach (`Copier.UnreachableSince`) until one finds it again; `DISK_FULL` when the last pass stopped on a full disk.
  `sameDisk` and `freeBytes` (null while the folder is out of reach) are read at each request.
- **`startCopy`** starts a full pass in the background, as `journal-server copy` does, unless one is going; a folder
  that isn't one of the caller's copies answers `INVALID_ARGUMENT`.
- **Progress in what the pass really copies**: a full pass first finds the originals the copy lacks (`counting`, a
  look at each one's size in the copy), so `running` has `totalFiles` and `totalBytes`, then `doneFiles` and
  `doneBytes`, the bytes counted as they are written (a large video moves the bar too).
- **`stopCopy`** stops the pass going now, in the middle of the original in hand: its temp file is removed, what was
  copied stays, and the pass ends `stopped` (logged as stopped from a phone). The next pass, daily or Copy now, goes
  on from there, since a pass copies only what the copy lacks.

## Code

- `copies/`: `Copier` (`Run` a full pass, `Check` the report's compare, `Changed` and `RunRealtime` for realtime
  copies), and `disk_*.go` for the same-disk check per OS. The copier reaches the copy only through a `Target`
  (`target.go`): `Folder` is a copy in a folder, `Peer` (`peer.go`) one on a friend's server ([peers.md](peers.md)).
  Every target passes `testTarget` in `target_test.go`.
- `copy.go`: making the copiers from config.json, the daily schedule, and the realtime start.
- `auth.CopyConfig`, and `checkCopies` in `auth/config.go`.
- The admin schema's `copyReport` and `startCopy`, and `admincli.go`'s `copy` commands.
- The public schema's `copies` and `startCopy` (`graph/resolver/copies.go`, through `copyRunner` in `copy.go`).
