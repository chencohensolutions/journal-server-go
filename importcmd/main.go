// Package importcmd is `journal-server import`: it copies the photos, videos and sound recordings found in folders on the server's disk into a user's data
// folder, once each: a file whose sha256 the data folder already has (from a device, an earlier run, or
// another copy found in this run) is not copied again. Every source file it is done with, copied or a
// duplicate, is then moved to the --imported folder under its original path, so that folder can be
// deleted as a whole, and whatever is left in the searched folders is what wasn't imported (see the report).
//
// Files are only copied, byte for byte (the copy's sha256 is checked): nothing is written into them. What
// is read from them (date, size, GPS) goes in the sidecar next to the copy, like an upload's facts.
//
// The date comes from the file name (phones name files by the local time they were taken), checked
// against the photo's EXIF (read with libvips' vipsheader, or from the DNG itself) or the video's own
// metadata (ffprobe), which add the time zone when they agree. Without a date in the name, the metadata
// is used. A time without an offset is taken as this machine's time zone. Files with no date are left
// where they are.
//
// Photos that look alike but aren't the same bytes are found by their dHash (within --near bits), then
// told apart by the same-picture check (same.go): both shrunk to the same small grey square and compared
// pixel by pixel, after checking they have the same shape and, when the same camera took both, were taken
// within 10 ms of each other. Another shot (a burst, a second try) is imported. The same picture (a
// resized or recompressed copy, its EXIF changed) is not, and goes to the not-imported folder's "same
// picture" folder; a larger copy of it, or a pair the check isn't sure of, waits there in "near-duplicates"
// for --review. Every pair, with its score, is listed in the run's near-duplicates.tsv. --recheck decides
// again on the photos waiting for review (those held back before this check, too).
//
// The dHash of every photo in the data folder is kept in its .cache/dhash.tsv, and the sha256 of every
// file looked at in .cache/source-hashes.tsv (by path, size and modified time), so the import after a dry
// run doesn't read everything again.
//
// The server may keep running: it and every import take turns at writing to the data folder (media/turns.go),
// and it sees imported files at once. Run from the server folder:
//
//	journal-server import --user hen --imported D:\Imported --not-imported "D:\Not imported" [--dry-run] D:\Backups "E:\Old phone"
//	journal-server import --user hen --imported D:\Imported --not-imported "D:\Not imported" --recheck [--dry-run]
package importcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/version"
)

// What is imported, by extension.
var mediaTypes = map[string]struct {
	kind media.Kind
	mime string
}{
	".jpg":  {media.KindPhoto, "image/jpeg"},
	".jpeg": {media.KindPhoto, "image/jpeg"},
	".heic": {media.KindPhoto, "image/heic"},
	".heif": {media.KindPhoto, "image/heif"},
	".png":  {media.KindPhoto, "image/png"},
	".webp": {media.KindPhoto, "image/webp"},
	".gif":  {media.KindPhoto, "image/gif"},
	".dng":  {media.KindPhoto, "image/x-adobe-dng"},
	".mp4":  {media.KindVideo, "video/mp4"},
	".mov":  {media.KindVideo, "video/quicktime"},
	".m4v":  {media.KindVideo, "video/x-m4v"},
	".3gp":  {media.KindVideo, "video/3gpp"},
	".mkv":  {media.KindVideo, "video/x-matroska"},
	".webm": {media.KindVideo, "video/webm"},
	".avi":  {media.KindVideo, "video/x-msvideo"},
	// Only the sound formats found so far; add others as they turn up. A WAV is kept as FLAC (audio.go).
	".m4a":  {media.KindAudio, "audio/mp4"},
	".aac":  {media.KindAudio, "audio/aac"},
	".flac": {media.KindAudio, "audio/flac"},
	".ogg":  {media.KindAudio, "audio/ogg"},
	".opus": {media.KindAudio, "audio/ogg"},
	".wav":  {media.KindAudio, "audio/wav"},
}

// Folders never searched, besides hidden ones (".thumbnails", ".cache") and the data and imported folders.
var skippedFolders = map[string]bool{"$recycle.bin": true, "system volume information": true}

type importer struct {
	store          *media.Store
	vips           string
	vipsheader     string
	ffprobe        string
	ffmpeg         string // encodes WAVs as FLAC
	workDir        string // the small previews dHash is made from
	user           string
	dataFolder     string
	importedFolder string
	runFolder      string // inside importedFolder, named by when this run started: what it moves, and its reports
	notImported    string
	notImportedRun string   // inside notImported, named like runFolder: the files this run didn't import, by why
	moveOther      bool     // also move files that aren't photos or videos, and hidden ones
	deleteDups     bool     // delete a source file whose bytes the data folder already has, instead of moving it
	roots          []string // the folders searched
	dryRun         bool
	nearDistance   int
	dhashes        *dhashTable
	sources        *sourceHashes
	cloud          *cloudImport // set when importing from Google Drive or OneDrive (cloud.go)

	mu           sync.Mutex      // everything below
	seen         map[string]bool // dry run: sha256s that would have been imported
	waiting      map[string]int  // sha256s of near-duplicates waiting for review: their index in toReview
	photos       []photo         // compared for near-duplicates
	counts       counts
	notMedia     map[string]int  // by extension
	leftBehind   []string        // report lines: files that stay where they are
	rejected     []string        // report lines: files moved to the not-imported folder
	others       []sourceFile    // files that aren't photos or videos, or are hidden: moved with --move-other
	deleted      []string        // report lines: duplicates deleted with --delete-duplicates
	verified     map[string]bool // sha256s whose original in the data folder was hashed again: whether it has those bytes
	dateNotes    []string
	near         []nearPair
	pictures     pictureCache // has its own lock
	toReview     []reviewItem
	lastProgress time.Time
	started      time.Time
	totalFiles   int
	totalBytes   int64
	doneFiles    int
	doneBytes    int64

	// The folder kinds the data folder's files and the run's new files take, counted once before the run places
	// anything (countFolderKinds); nil before, when each file is decided by the data folder's files alone.
	folderKinds *media.FolderKinds
}

// photo is one photo in the data folder (or on its way there, or in a dry run one that would be), with
// its dHash.
type photo struct {
	hash   dhash
	sha256 string
	path   string // where it was found, for one from this run (the data folder's is looked up by sha256)
}

// nearPair is a photo found to look like one before it, and what the same-picture check made of them.
type nearPair struct {
	distance int // dHash bits apart
	verdict  verdict
	better   bool    // the same picture, with more pixels than the one it looks like
	score    float64 // the pixels' PSNR in dB; 0 when not compared
	why      string
	path     string // the new photo, where it was found
	like     photo
}

type counts struct {
	imported, duplicates, same, noDate, failed, hidden int
	importedBytes, duplicateBytes                      int64
	deleted                                            int // duplicates deleted (--delete-duplicates)
	emptyFolders                                       int // folders left empty in the searched folders, deleted
	deletedBytes                                       int64
	audio                                              map[string]int // sound recordings imported, by media.AudioCollectionOf
	encoded                                            int            // WAVs kept as FLAC
}

// Main runs `journal-server import` with the arguments after it. Its own flags; it may exit the process.
func Main(args []string) {
	flags := flag.NewFlagSet("journal-server import", flag.ExitOnError)
	user := flags.String("user", "", "user whose data folder the files go to (from config.json)")
	imported := flags.String("imported", "", "folder the source files are moved to once imported (or found to be duplicates)")
	dryRun := flags.Bool("dry-run", false, "only count what would be imported; nothing is copied or moved")
	jobs := flags.Int("jobs", 4, "files worked on at once")
	near := flags.Int("near", 8, "most dHash bits two photos may differ in to be compared as near-duplicates (0–64)")
	review := flags.Bool("review", false, "show the near-duplicates waiting in the imported folder side by side, to import or skip each")
	recheck := flags.Bool("recheck", false, "decide again on the near-duplicates waiting for review, with the same-picture check")
	noBrowser := flags.Bool("no-browser", false, "with --review, only print the page's address")
	notImported := flags.String("not-imported", "", "folder the files that weren't imported are moved to, by why (required to import)")
	deleteDups := flags.Bool("delete-duplicates", false, "delete a source file the data folder already has (same sha256, both hashed again first) instead of moving it to the imported folder; nothing in the data folder is ever deleted")
	moveOther := flags.Bool("move-other", false, "also move files that aren't photos or videos, and hidden ones (.trashed-…), to the not-imported folder")
	backfill := flags.Bool("backfill-folders", false, "only add the folders of files looked at by earlier runs (from .cache/source-hashes.tsv) to their sidecars")
	cloud := flags.String("cloud", "", "import from the cloud instead of folders: gdrive, onedrive, or gdrive,onedrive (through rclone; see cloud.go)")
	temp := flags.String("temp", "", "with --cloud, the folder files are downloaded to before they are imported (emptied as they are)")
	batchGB := flags.Float64("batch-gb", 20, "with --cloud, about how much is downloaded at a time")
	rclonePath := flags.String("rclone", "", "with --cloud, the rclone program (found on the PATH or where winget links it when empty)")
	configPath := flags.String("config", "config.json", "the server's config file")
	showVersion := flags.Bool("version", false, "print the version and exit")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: journal-server import --user <name> --imported <folder> --not-imported <folder> [--dry-run] [--move-other] <folder>...")
		fmt.Fprintln(os.Stderr, "       journal-server import --user <name> --imported <folder> --not-imported <folder> --cloud gdrive,onedrive --temp <folder> [--dry-run]")
		fmt.Fprintln(os.Stderr, "       journal-server import --user <name> --imported <folder> --not-imported <folder> --recheck [--dry-run]")
		fmt.Fprintln(os.Stderr, "       journal-server import --user <name> --imported <folder> --review")
		fmt.Fprintln(os.Stderr, "       journal-server import --user <name> --imported <folder> --backfill-folders <folder>...")
		flags.PrintDefaults()
	}
	flags.Parse(args)
	if *showVersion {
		fmt.Println("journal-server", version.String())
		return
	}
	if *user == "" || *imported == "" || (flags.NArg() == 0) != (*review || *recheck || *cloud != "") || *jobs < 1 ||
		(*cloud != "") != (*temp != "") || *batchGB <= 0 {
		flags.Usage()
		os.Exit(2)
	}

	// Installed, journal-server import may run from any folder: the folders given are taken from here before it
	// changes to the folder of its config.json (auth.UseInstallFolder).
	args = flags.Args()
	for i := range args {
		args[i] = absolute(args[i])
	}
	for _, p := range []*string{imported, notImported, temp} {
		if *p != "" {
			*p = absolute(*p)
		}
	}
	_, err := auth.UseInstallFolder(*configPath)
	check(err)
	config, err := auth.LoadConfig(*configPath)
	check(err)
	dataFolder, err := config.DataFolderOf(*user)
	check(err)
	vips, vipsheader, err := media.FindVips(config.Vips)
	if err != nil {
		log.Fatalf("%v (brew install vips; on Windows go run ./scripts/bundle-vips; or set \"vips\" in config.json)", err)
	}
	ffmpeg, ffprobe, err := media.FindFFmpeg(config.FFmpeg)
	if err != nil {
		log.Fatalf("%v (brew install ffmpeg; on Windows winget install Gyan.FFmpeg; or set \"ffmpeg\" in config.json)", err)
	}

	if !*review && !*backfill && *notImported == "" {
		log.Fatal("--not-imported: the folder for the files that aren't imported (for example \"D:\\Not imported\")")
	}
	started := claimRunName(absolute(*imported), time.Now().Format("2006-01-02-15-04-05"))
	im := &importer{
		vips:           vips,
		vipsheader:     vipsheader,
		ffprobe:        ffprobe,
		ffmpeg:         ffmpeg,
		user:           *user,
		dataFolder:     absolute(dataFolder),
		importedFolder: absolute(*imported),
		runFolder:      filepath.Join(absolute(*imported), started),
		moveOther:      *moveOther,
		deleteDups:     *deleteDups,
		verified:       map[string]bool{},
		dryRun:         *dryRun,
		nearDistance:   *near,
		seen:           map[string]bool{},
		waiting:        map[string]int{},
		notMedia:       map[string]int{},
		counts:         counts{audio: map[string]int{}},
		lastProgress:   time.Now(),
		pictures:       pictureCache{pictures: map[string]*picture{}},
	}
	if *notImported != "" {
		im.notImported = absolute(*notImported)
		im.notImportedRun = filepath.Join(im.notImported, started)
	}
	roots := make([]string, flags.NArg())
	for i, root := range args {
		roots[i] = root
		if im.excluded(roots[i]) {
			log.Fatalf("%s is inside the data folder, the imported folder or the not-imported folder", root)
		}
	}
	im.roots = roots
	im.store, err = media.OpenStore(im.dataFolder, time.Now)
	check(err)
	im.dhashes, err = openDHashTable(im.dataFolder)
	check(err)
	defer im.dhashes.close()
	im.sources, err = openSourceHashes(im.dataFolder)
	check(err)
	defer im.sources.close()
	im.workDir, err = os.MkdirTemp("", "journal-import-")
	check(err)
	if !im.dryRun {
		check(im.store.ClearImports())
	}
	// Files other imports, the server or a scan place meanwhile are compared with as well.
	im.store.OnCatchUp(im.learn)
	defer os.RemoveAll(im.workDir)
	defer func() {
		// A run that moved nothing and wrote no report leaves no folders behind.
		os.Remove(im.runFolder)
		if im.notImportedRun != "" {
			os.RemoveAll(im.encodingDir())
			os.Remove(im.notImportedRun)
		}
	}()

	// Ctrl+C finishes the files in hand, then stops; running again continues where it stopped.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *review {
		im.review(ctx, !*noBrowser)
		return
	}
	if *backfill {
		im.backfillFolders()
		return
	}
	if *recheck {
		im.recheck(ctx, *jobs)
		return
	}

	usage := im.store.Usage(media.View{})
	log.Printf("Data folder %s: %d photos, %d videos, %d sound recordings", im.dataFolder, usage.Photos, usage.Videos, usage.Audio)
	im.hashDataFolder(ctx, *jobs)
	if im.dryRun {
		log.Print("Dry run: nothing is copied or moved")
	} else if *cloud == "" {
		log.Printf("Moving what is done to %s", im.runFolder)
	}

	if *cloud != "" {
		rclone, err := findRclone(*rclonePath)
		check(err)
		tempFolder := absolute(*temp)
		if im.excluded(tempFolder) {
			log.Fatalf("--temp %s is inside the data folder, the imported folder or the not-imported folder", *temp)
		}
		im.importCloud(ctx, strings.Split(*cloud, ","), rclone, tempFolder, int64(*batchGB*(1<<30)), *jobs)
		im.summarize()
		im.addToReview()
		return
	}

	// List everything first, so progress can be shown against the total.
	var files []sourceFile
	for _, root := range roots {
		found, err := im.walk(ctx, root)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Fatal("Stopped")
			}
			log.Printf("%s: %v", root, err)
		}
		files = append(files, found...)
	}
	for _, f := range files {
		im.totalBytes += f.size
	}
	im.totalFiles = len(files)
	log.Printf("%d photos, videos and sound recordings to look at, %s", im.totalFiles, gb(im.totalBytes))
	im.importFiles(ctx, files, *jobs)
	if ctx.Err() != nil {
		log.Print("Stopped; running again continues where it stopped")
	}
	if im.moveOther && ctx.Err() == nil {
		im.moveOthers()
	}
	im.removeEmptyFolders()
	im.summarize()
	im.addToReview()
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
