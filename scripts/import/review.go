package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/journal/server-go/media"
)

// reviewItem is a near-duplicate left where it was found for the owner to decide on, kept in the imported
// folder's review.json until then.
type reviewItem struct {
	Path    string `json:"path"`              // the new photo, now (in the not-imported folder)
	FoundAt string `json:"foundAt,omitempty"` // where it was found
	// The folders of identical copies found after it (moved to the imported folder as its duplicates).
	AlsoFoundIn []string `json:"alsoFoundIn,omitempty"`
	Sha256      string   `json:"sha256"`
	Root        string   `json:"root"` // the searched folder it was found in
	LikeSha256  string   `json:"likeSha256"`
	LikePath    string   `json:"likePath,omitempty"` // when the look-alike isn't in the data folder
	Distance    int      `json:"distance"`
}

// folders are the folders the photo and its identical copies were found in.
func (item reviewItem) folders() []string {
	var folders []string
	if folder := folderOf(item.Root, item.foundAt()); folder != "" {
		folders = append(folders, folder)
	}
	return append(folders, item.AlsoFoundIn...)
}

// foundAt is where the photo was found (older lists have only Path).
func (item reviewItem) foundAt() string {
	if item.FoundAt != "" {
		return item.FoundAt
	}
	return item.Path
}

func (im *importer) reviewFile() string { return filepath.Join(im.importedFolder, "review.json") }

func (im *importer) loadReview() ([]reviewItem, error) {
	data, err := os.ReadFile(im.reviewFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []reviewItem
	return items, json.Unmarshal(data, &items)
}

func (im *importer) saveReview(items []reviewItem) error {
	if len(items) == 0 {
		if err := os.Remove(im.reviewFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(im.importedFolder, 0o755); err != nil {
		return err
	}
	tmp := im.reviewFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, im.reviewFile())
}

// addToReview adds this run's near-duplicates to the ones waiting from earlier runs.
func (im *importer) addToReview() {
	if len(im.toReview) == 0 {
		return
	}
	im.withTurn(im.addToReviewInTurn)
}

func (im *importer) addToReviewInTurn() {
	items, err := im.loadReview()
	if err != nil {
		log.Printf("%s: %v; this run's near-duplicates are only in the near-duplicates report", im.reviewFile(), err)
		return
	}
	known := map[string]bool{}
	for _, item := range items {
		known[strings.ToLower(item.foundAt())] = true
	}
	for _, item := range im.toReview {
		if !known[strings.ToLower(item.foundAt())] {
			items = append(items, item)
		}
	}
	if err := im.saveReview(items); err != nil {
		log.Printf("%s: %v", im.reviewFile(), err)
		return
	}
	fmt.Printf("%d near-duplicates wait for you. To see them side by side and decide:\n  go run ./scripts/import --user %s --imported \"%s\" --review\n", len(items), im.user, im.importedFolder)
}

// review serves a page on this machine showing each near-duplicate next to the photo it looks like, to
// import it too or skip it (moved to the imported folder, like a duplicate). Ctrl+C ends it.
func (im *importer) review(ctx context.Context, browser bool) {
	items, err := im.loadReview()
	check(err)
	if len(items) == 0 {
		fmt.Println("No near-duplicates wait for a decision.")
		return
	}
	r := &reviewer{im: im, items: items}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	url := "http://" + listener.Addr().String() + "/"
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(reviewPage)
	})
	mux.HandleFunc("GET /items", r.list)
	mux.HandleFunc("GET /info", r.info)
	mux.HandleFunc("GET /preview", r.preview)
	mux.HandleFunc("POST /decide", r.decide)
	server := &http.Server{Handler: mux}
	go func() {
		<-ctx.Done()
		server.Close()
	}()
	fmt.Printf("%d near-duplicates to decide on: %s (Ctrl+C when done)\n", len(items), url)
	if browser {
		openBrowser(url)
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

type reviewer struct {
	im    *importer
	mu    sync.Mutex
	items []reviewItem
}

func (r *reviewer) find(sha string) (reviewItem, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range r.items {
		if item.Sha256 == sha {
			return item, true
		}
	}
	return reviewItem{}, false
}

// paths returns the new photo's path and its look-alike's (in the data folder, or where it was found).
func (r *reviewer) paths(item reviewItem) (newPath, likePath string, like *media.Item) {
	likePath = item.LikePath
	if found, ok := r.im.store.Get(item.LikeSha256); ok {
		likePath = filepath.Join(r.im.dataFolder, filepath.FromSlash(found.Path))
		like = &found
	}
	return item.Path, likePath, like
}

func (r *reviewer) list(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	type entry struct {
		ID       string `json:"id"`
		Distance int    `json:"distance"`
	}
	entries := make([]entry, len(r.items))
	for i, item := range r.items {
		entries[i] = entry{item.Sha256, item.Distance}
	}
	r.mu.Unlock()
	writeJSON(w, entries)
}

// info describes one side of a pair: rows of label and value, in the same order on both sides.
func (r *reviewer) info(w http.ResponseWriter, req *http.Request) {
	item, ok := r.find(req.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, req)
		return
	}
	newPath, likePath, like := r.paths(item)
	path := newPath
	if req.URL.Query().Get("side") == "like" {
		path = likePath
	}
	type row struct{ Label, Value string }
	rows := []row{{"Name", filepath.Base(path)}, {"Folder", filepath.Dir(path)}}
	if path == newPath {
		rows[1] = row{"Found in", filepath.Dir(item.foundAt())}
	}
	stat, err := os.Stat(path)
	if err != nil {
		writeJSON(w, append(rows, row{"Missing", err.Error()}))
		return
	}
	f := r.im.readFacts(req.Context(), path)
	rows = append(rows, row{"File size", fmt.Sprintf("%.2f MB", float64(stat.Size())/(1<<20))})
	if f.width != nil {
		megapixels := float64(*f.width) * float64(*f.height) / 1e6
		rows = append(rows, row{"Pixels", fmt.Sprintf("%d × %d (%.1f MP)", *f.width, *f.height, megapixels)})
	}
	if f.takenAt != nil {
		rows = append(rows, row{"Taken", f.takenAt.Format("Mon 2 Jan 2006, 15:04:05 -07:00")})
	}
	if original := f.exif["exif-ifd2-DateTimeOriginal"]; original != "" {
		rows = append(rows, row{"EXIF date", original + " " + f.exif["exif-ifd2-OffsetTimeOriginal"]})
	}
	camera := strings.TrimSpace(f.exif["exif-ifd0-Make"] + " " + f.exif["exif-ifd0-Model"])
	rows = append(rows, row{"Camera", camera}, row{"Software", f.exif["exif-ifd0-Software"]})
	if f.location != nil {
		rows = append(rows, row{"Place", fmt.Sprintf("%.5f, %.5f", f.location.Lat, f.location.Lon)})
	} else {
		rows = append(rows, row{"Place", ""})
	}
	if path == likePath && like != nil {
		facts := like.Sidecar.Facts
		rows = append(rows, row{"In the journal", fmt.Sprintf("since %s, from %s", facts.UploadedAt.Local().Format("2 Jan 2006 15:04"), facts.UploadedBy)})
		if facts.ImportedFrom != nil {
			rows = append(rows, row{"Imported from", *facts.ImportedFrom})
		}
	}
	rows = append(rows, row{"sha256", item.Sha256})
	if path == likePath {
		rows[len(rows)-1].Value = item.LikeSha256
	}
	writeJSON(w, rows)
}

// preview is the photo as a JPEG at most 1600 pixels on its long side, made by libvips (browsers can't
// show HEIC or DNG).
func (r *reviewer) preview(w http.ResponseWriter, req *http.Request) {
	item, ok := r.find(req.URL.Query().Get("id"))
	if !ok {
		http.NotFound(w, req)
		return
	}
	side := req.URL.Query().Get("side")
	newPath, likePath, _ := r.paths(item)
	path, key := newPath, item.Sha256
	if side == "like" {
		path, key = likePath, item.LikeSha256
	}
	out := filepath.Join(r.im.workDir, key+".jpg")
	if _, err := os.Stat(out); err != nil {
		cmd := exec.CommandContext(req.Context(), r.im.vips, "thumbnail", path, out+"[Q=85]", "1600", "--height", "1600")
		if msg, err := cmd.CombinedOutput(); err != nil {
			os.Remove(out)
			http.Error(w, strings.TrimSpace(string(msg)), http.StatusUnprocessableEntity)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, req, out)
}

// decide imports the new photo too, or skips it; either way it moves to the imported folder and leaves
// the list.
func (r *reviewer) decide(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock() // one decision at a time
	defer r.mu.Unlock()
	r.im.withTurn(func() { r.decideInTurn(w, req) })
}

func (r *reviewer) decideInTurn(w http.ResponseWriter, req *http.Request) {
	// Imports running meanwhile may have added near-duplicates to the list.
	if items, err := r.im.loadReview(); err == nil {
		r.items = items
	}
	sha, action := req.URL.Query().Get("id"), req.URL.Query().Get("action")
	index := -1
	for i, item := range r.items {
		if item.Sha256 == sha {
			index = i
		}
	}
	if index < 0 || (action != "import" && action != "skip") {
		http.Error(w, "unknown photo or action", http.StatusBadRequest)
		return
	}
	item := r.items[index]
	im := r.im
	message := ""
	info, err := os.Stat(item.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		message = "The file isn't there any more; removed from the list."
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	case action == "import":
		current, err := im.sources.sha256(item.Path, info)
		if err != nil || current != item.Sha256 {
			http.Error(w, fmt.Sprintf("the file changed since it was found (%v); run the import again", err), http.StatusConflict)
			return
		}
		facts := im.readFacts(req.Context(), item.Path)
		if facts.takenAt == nil {
			http.Error(w, "no date in its file name or metadata", http.StatusUnprocessableEntity)
			return
		}
		placed, existed, err := im.importInto(item.Path, item.Sha256, info.Size(), facts, item.folders())
		if err != nil {
			http.Error(w, "not imported: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if !existed {
			im.wrote(placed)
		}
		message = "Imported as " + placed.Path
		fallthrough
	default:
		if _, err := moveFile(item.Path, filepath.Join(im.runFolder, pathInside([]string{item.Root}, item.foundAt()))); err != nil {
			http.Error(w, message+"; not moved to the imported folder: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if action == "skip" {
			message = "Skipped; moved to the imported folder."
			// Its folder is a clue about the photo it is a copy of.
			if folders := item.folders(); len(folders) > 0 {
				if changed, err := im.store.AddFolders(item.LikeSha256, folders); err == nil && changed {
					if like, ok := im.store.Get(item.LikeSha256); ok {
						im.wrote(like)
					}
					message += " Its folders were added to the look-alike's: " + strings.Join(folders, ", ") + "."
				}
			}
		}
	}
	r.items = append(r.items[:index:index], r.items[index+1:]...)
	if err := im.saveReview(r.items); err != nil {
		log.Printf("%s: %v", im.reviewFile(), err)
	}
	os.MkdirAll(im.runFolder, 0o755)
	appendLine(filepath.Join(im.runFolder, "review-decisions.tsv"),
		strings.Join([]string{time.Now().Format(time.RFC3339), action, item.Path, item.Sha256}, "\t"))
	log.Printf("%s: %s", item.Path, message)
	writeJSON(w, map[string]any{"message": message, "left": len(r.items)})
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Print(err)
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(value)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open %s in a browser (%v)", url, err)
	}
}

//go:embed review.html
var reviewPage []byte
