// Command fix-sources corrects, once, the source of files WhatsApp named (IMG-20230115-WA0012.jpg) that came in
// as camera or other, so the apps can hide them with the rest of WhatsApp. scripts/import used to tell the source
// from the folder only, so a WhatsApp file copied elsewhere before it was imported lost it; it now looks at the
// name too (media.SourceOfPath).
//
// It also gives screenshots placed before the server read their names the facts new ones get
// (media.ScreenshotApp): source screenshots, and the app on screen (Screenshot_20240301_030715_Bumble.jpg → app
// "Bumble"), so a whole app's screenshots can be told apart. Only the sidecar's facts.source and facts.app change
// (and facts.correctedAt, so devices get the file again in the change feed); originals are never touched.
//
// It prints where the corrected files were found (the first folders of facts.importedFrom), to tell why they
// had the wrong source, and also lists files whose folders mention WhatsApp but whose source isn't whatsapp,
// without changing those. The server may keep running: they take turns at writing (media/turns.go).
// Run from the server folder, first with --dry-run:
//
//	go run ./scripts/fix-sources --user hen [--dry-run]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

func main() {
	user := flag.String("user", "", "user whose data folder is corrected (from config.json)")
	dryRun := flag.Bool("dry-run", false, "only say what would change")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/fix-sources --user <name> [--dry-run]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if *user == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}

	config, err := auth.LoadConfig(*configPath)
	check(err)
	dataFolder, err := config.DataFolderOf(*user)
	check(err)
	store, err := media.OpenStore(dataFolder, time.Now)
	check(err)
	r, err := fix(store, *dryRun)
	check(err)
	apps, err := fixScreenshots(store, *dryRun)
	check(err)

	verb := "corrected"
	if *dryRun {
		verb = "would be corrected (dry run)"
	}
	for _, line := range r.folderOnly {
		fmt.Println("not changed (only its folders say WhatsApp): " + line)
	}
	fmt.Printf("%d files, %d named by WhatsApp %s to whatsapp", r.files, r.fixed, verb)
	if r.fixed > 0 {
		fmt.Printf(", from: %s", counts(r.was))
	}
	fmt.Println()
	if r.fixed > 0 {
		fmt.Println("where they were found:")
		for _, line := range sortedCounts(r.foundIn) {
			fmt.Println("  " + line)
		}
	}
	if len(r.folderOnly) > 0 {
		fmt.Printf("%d files left as they are: only their folders mention WhatsApp\n", len(r.folderOnly))
	}
	screenshots := 0
	for _, n := range apps {
		screenshots += n
	}
	fmt.Printf("%d screenshots %s with their app\n", screenshots, verb)
	for _, line := range sortedCounts(apps) {
		fmt.Println("  " + line)
	}
}

// report is what fix found: how many files it corrected, their sources before and where they were found, and
// the files it left as they are though their folders mention WhatsApp.
type report struct {
	files, fixed int
	was, foundIn map[string]int
	folderOnly   []string
}

// fix sets the source of every file WhatsApp named to whatsapp, unless dryRun.
func fix(store *media.Store, dryRun bool) (report, error) {
	items, _, _, err := store.Changes("", 1<<31-1, media.View{})
	if err != nil {
		return report{}, err
	}
	r := report{files: len(items), was: map[string]int{}, foundIn: map[string]int{}}
	for _, item := range items {
		facts := item.Sidecar.Facts
		source := media.SourceOther
		if facts.Source != nil {
			source = *facts.Source
		}
		if source == media.SourceWhatsApp {
			continue
		}
		if !media.IsWhatsAppName(facts.FileName) {
			if mentionsWhatsApp(facts) {
				r.folderOnly = append(r.folderOnly, fmt.Sprintf("%s  %s  %v", item.Path, source, facts.Folders))
			}
			continue
		}
		r.fixed++
		r.was[source]++
		r.foundIn[firstFolders(facts)]++
		if !dryRun {
			if _, err := store.SetSource(item.Sha256(), media.SourceWhatsApp); err != nil {
				return r, err
			}
		}
	}
	return r, nil
}

// fixScreenshots gives every file named like a screenshot the source screenshots and the app its name says, unless
// dryRun. It returns how many it corrected (or would) for each app ("" for names without one).
func fixScreenshots(store *media.Store, dryRun bool) (map[string]int, error) {
	items, _, _, err := store.Changes("", 1<<31-1, media.View{})
	if err != nil {
		return nil, err
	}
	apps := map[string]int{}
	for _, item := range items {
		facts := item.Sidecar.Facts
		app, ok := media.ScreenshotApp(facts.FileName)
		if !ok {
			continue
		}
		if facts.Source != nil && *facts.Source == media.SourceScreenshots && (app == "" || facts.ScreenshotApp != nil && *facts.ScreenshotApp == app) {
			continue
		}
		if !dryRun {
			if _, err := store.SetScreenshotFacts(item.Sha256()); err != nil {
				return apps, err
			}
		}
		apps[app]++
	}
	return apps, nil
}

func mentionsWhatsApp(facts media.Facts) bool {
	if facts.ImportedFrom != nil && strings.Contains(strings.ToLower(*facts.ImportedFrom), "whatsapp") {
		return true
	}
	for _, f := range facts.Folders {
		if strings.Contains(strings.ToLower(f), "whatsapp") {
			return true
		}
	}
	return false
}

// firstFolders is up to the first five folders of where the file was imported from ("uploaded from a device"
// when it wasn't imported), to group the files by why they had the wrong source.
func firstFolders(facts media.Facts) string {
	if facts.ImportedFrom == nil {
		return "uploaded from a device"
	}
	dir := path.Dir(strings.ReplaceAll(*facts.ImportedFrom, `\`, "/"))
	parts := strings.Split(dir, "/")
	if len(parts) > 5 {
		parts = append(parts[:5], "…")
	}
	return strings.Join(parts, "/")
}

func counts(m map[string]int) string { return strings.Join(sortedCounts(m), ", ") }

// sortedCounts is "<n> <key>" for each key, the largest first.
func sortedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || m[keys[i]] == m[keys[j]] && keys[i] < keys[j] })
	lines := make([]string, len(keys))
	for i, k := range keys {
		lines[i] = fmt.Sprintf("%d %s", m[k], k)
	}
	return lines
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
