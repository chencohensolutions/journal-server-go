// Command fix-sources gives every file in a user's data folder the source the server's one rule gives it
// (media.SourceOf: a screenshot's name, WhatsApp, a messaging app, the camera's tags and the user's camera marks, a
// Camera folder or name, a named folder; see docs/media.md "Sources"), and screenshots the app their name says. It
// is for files placed before the rule was what it is now: imports labelled other that came from a camera, and
// whatsapp, which is folder:WhatsApp since Oct 27, 2026. Only a sidecar's facts.source, facts.screenshotApp and
// facts.correctedAt change (facts.sentFrom keeps the source it had, when it kept none), one turn and one rewrite per
// file, so each file comes once again in the change feed; originals, notes, nudity and privacy are never touched.
// settings.json's hidden.sources get the new ids too.
//
// A real run writes a journal, relabels/<UTC time>.tsv at the data folder's root, with every value it changed, as
// it goes; --undo <journal> puts them back. --to-old-ids writes the ids an older server and app know, for going back
// to one (also journaled). --dry-run writes nothing at all, not even the caches, so it is safe beside the running
// server. A real run may also run beside it, and beside imports: they take turns at writing (media/turns.go). Run
// from the server folder, after scripts/read-cameras, first with --dry-run:
//
//	go run ./scripts/fix-sources --user hen --dry-run [--list other:camera]
//	go run ./scripts/fix-sources --user hen
//	go run ./scripts/fix-sources --user hen --undo relabels/2026-10-27T134009Z.tsv
//	go run ./scripts/fix-sources --user hen --to-old-ids
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/media"
)

type options struct {
	user, undo, list string
	dryRun, oldIds   bool
}

func main() {
	var o options
	flag.StringVar(&o.user, "user", "", "user whose data folder is relabelled (from config.json)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "only say what would change; writes nothing")
	flag.StringVar(&o.list, "list", "", "with a dry run, list up to 20 files of one change, <before>:<after> (other:camera)")
	flag.StringVar(&o.undo, "undo", "", "put back what the journal of a real run changed")
	flag.BoolVar(&o.oldIds, "to-old-ids", false, "write the ids an older server knows: folder:WhatsApp as whatsapp, received as camera, screenshotApp as app")
	configPath := flag.String("config", "config.json", "the server's config file")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: go run ./scripts/fix-sources --user <name> [--dry-run] [--list <before>:<after>] [--undo <journal> | --to-old-ids]")
		flag.PrintDefaults()
	}
	flag.Parse()
	if o.user == "" || flag.NArg() != 0 || o.undo != "" && o.oldIds {
		flag.Usage()
		os.Exit(2)
	}
	if o.list != "" {
		o.dryRun = true
	}

	config, err := auth.LoadConfig(*configPath)
	check(err)
	dataFolder, err := config.DataFolderOf(o.user)
	check(err)
	open := media.OpenStore
	if o.dryRun {
		open = media.OpenStoreReadOnly
	}
	store, err := open(dataFolder, time.Now)
	check(err)
	check(run(store, o, os.Stdout, time.Now))
}

// run plans the change, prints it, and unless it is a dry run makes it.
func run(store *media.Store, o options, out io.Writer, now func() time.Time) error {
	mode := modeRelabel
	var undo []journalLine
	switch {
	case o.undo != "":
		mode = modeUndo
		var err error
		if undo, err = readJournal(findJournal(store.Root(), o.undo)); err != nil {
			return err
		}
	case o.oldIds:
		mode = modeOldIds
	}
	p, err := newPlan(store, mode, undo)
	if err != nil {
		return err
	}
	if o.list != "" {
		return p.printList(out, o.list)
	}
	p.print(out, o.dryRun)
	if o.dryRun {
		return nil
	}
	j := newJournal(store, o.user, mode, now)
	files, err := p.apply(store, j)
	if cerr := j.close(); err == nil {
		err = cerr
	}
	if j.path != "" {
		fmt.Fprintf(out, "journal: %s (undo with --undo)\n", j.path)
	}
	if err != nil {
		return fmt.Errorf("stopped after %s files: %w", count(files), err)
	}
	fmt.Fprintf(out, "%s files changed\n", count(files))
	return nil
}

// transition is a change of source, as "<before> → <after>"; a change of only the app, or of the key it is under,
// is a transition from a source to itself.
func transition(c change) (before, after string) {
	return idName(c.before.Source), idName(c.after.Source)
}

func idName(id string) string {
	if id == "" {
		return "(none)"
	}
	return id
}

// print says what the plan changes: how many files from each source to each, where they were found, what stays
// other and settings.json's ids.
func (p *plan) print(out io.Writer, dryRun bool) {
	verb := "change"
	if dryRun {
		verb = "would change (dry run, nothing written)"
	}
	fmt.Fprintf(out, "%s: %s files read, %s %s\n", p.mode, count(p.files), count(len(p.changes)), verb)
	if p.unread > 0 {
		fmt.Fprintf(out, "%s files left out (see the log)\n", count(p.unread))
	}
	groups := map[string][]change{}
	apps, keys := map[string]int{}, 0
	for _, c := range p.changes {
		before, after := transition(c)
		key := before + " → " + after
		groups[key] = append(groups[key], c)
		if c.before.ScreenshotApp != c.after.ScreenshotApp {
			apps[idName(c.after.ScreenshotApp)]++
		}
		if keyOf(c.before) != keyOf(c.after) && c.before.ScreenshotApp == c.after.ScreenshotApp {
			keys++
		}
	}
	sizes := map[string]int{}
	for key, cs := range groups {
		sizes[key] = len(cs)
	}
	for _, key := range sortedKeys(sizes) {
		fmt.Fprintf(out, "\n%s: %s\n", key, count(sizes[key]))
		where := map[string]int{}
		for _, c := range groups[key] {
			where[foundIn(c.item.Sidecar.Facts)]++
		}
		printTop(out, where, 5)
	}
	if len(apps) > 0 {
		fmt.Fprintln(out, "\nscreenshots' apps set:")
		printTop(out, apps, 10)
	}
	if keys > 0 {
		fmt.Fprintf(out, "\n%s screenshots' apps written under another key only\n", count(keys))
	}
	if p.mode == modeRelabel {
		fmt.Fprintf(out, "\nstays other: %s\n", count(len(p.stayOther)))
		where := map[string]int{}
		for _, item := range p.stayOther {
			where[foundIn(item.Sidecar.Facts)]++
		}
		printTop(out, where, 30)
	}
	for _, c := range p.settings {
		fmt.Fprintf(out, "\nsettings.json hidden.sources: %s → %s", idName(c.old), idName(c.new))
	}
	if len(p.settings) > 0 {
		fmt.Fprintln(out)
	}
}

// printList prints up to 20 files of one change, given as <before>:<after> (ids have colons themselves, so every
// split that names a change in the plan is tried).
func (p *plan) printList(out io.Writer, which string) error {
	for i := range len(which) {
		if which[i] != ':' {
			continue
		}
		want := [2]string{which[:i], which[i+1:]}
		var matched []change
		for _, c := range p.changes {
			before, after := transition(c)
			if strings.EqualFold(before, want[0]) && strings.EqualFold(after, want[1]) {
				matched = append(matched, c)
			}
		}
		if len(matched) == 0 {
			continue
		}
		fmt.Fprintf(out, "%s → %s: %s files", want[0], want[1], count(len(matched)))
		if len(matched) > 20 {
			fmt.Fprint(out, ", the first 20")
			matched = matched[:20]
		}
		fmt.Fprintln(out)
		for _, c := range matched {
			fmt.Fprintf(out, "  %s  (%s)\n", c.item.Path, foundIn(c.item.Sidecar.Facts))
		}
		return nil
	}
	return fmt.Errorf("no change %q in this run (give it as <before>:<after>, as the dry run names them)", which)
}

// foundIn is where a file was found, to group files by why they had their source: up to the first five folders of
// where the import found it, the uploading device's folder, or the first folder it was found in.
func foundIn(facts media.Facts) string {
	switch {
	case facts.ImportedFrom != nil:
		dir := path.Dir(strings.ReplaceAll(*facts.ImportedFrom, `\`, "/"))
		parts := strings.Split(dir, "/")
		if len(parts) > 5 {
			parts = append(parts[:5], "…")
		}
		return strings.Join(parts, "/")
	case facts.DeviceFolder != nil:
		return "device folder " + *facts.DeviceFolder
	case len(facts.Folders) > 0:
		return facts.Folders[0]
	case facts.Imported():
		return "imported, folder unknown"
	}
	return "uploaded from a device"
}

// printTop prints the n largest counts, and how many others there are.
func printTop(out io.Writer, m map[string]int, n int) {
	keys := sortedKeys(m)
	rest := 0
	for i, k := range keys {
		if i < n {
			fmt.Fprintf(out, "  %8s  %s\n", count(m[k]), k)
		} else {
			rest += m[k]
		}
	}
	if rest > 0 {
		fmt.Fprintf(out, "  %8s  in %d other places\n", count(rest), len(keys)-n)
	}
}

// sortedKeys are m's keys, the largest count first.
func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]] > m[keys[j]] || m[keys[i]] == m[keys[j]] && keys[i] < keys[j] })
	return keys
}

// count is n with thousands separated: 28,900.
func count(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
