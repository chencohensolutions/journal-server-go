package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/journal/server-go/media"
)

// What a run does: relabel every file by the rule, undo a journal, or write the ids an older server knows.
const (
	modeRelabel = "relabel"
	modeUndo    = "undo"
	modeOldIds  = "to-old-ids"
)

// decider is what a run makes of one file's labels on disk: the labels to write, and whether they differ. It is
// asked once when the run is planned and again in the file's turn, with what is on disk then.
type decider func(item media.Item, disk media.Labels) (media.Labels, bool)

// sourcesMapper is what a run makes of settings.json's hidden.sources: the ones to write, and each id it changes.
type sourcesMapper func(sources []string) ([]string, []idChange)

type idChange struct{ old, new string }

// change is one file a run changes (or, in a dry run, would).
type change struct {
	item          media.Item
	before, after media.Labels
}

// plan is what a run changes: the files, settings.json's ids, and for a relabel the files that stay other.
type plan struct {
	mode      string
	files     int
	changes   []change
	settings  []idChange
	stayOther []media.Item
	unread    int // sidecars that couldn't be read (logged), or journal lines for files no longer there
	decide    decider
	sources   sourcesMapper
}

// newPlan decides what a run changes, reading only: each file's labels on disk, and settings.json.
func newPlan(store *media.Store, mode string, undo []journalLine) (*plan, error) {
	p := &plan{mode: mode}
	var items []media.Item
	switch mode {
	case modeRelabel:
		p.decide, p.sources = relabelDecider(store.CameraMarks(), store.FolderKinds()), relabelSources
		store.Range(func(item media.Item) { items = append(items, item) })
	case modeOldIds:
		p.decide, p.sources = oldIdsDecider, oldIdsSources
		store.Range(func(item media.Item) { items = append(items, item) })
	case modeUndo:
		p.decide, p.sources = undoDecider(undo), undoSources(undo)
		seen := map[string]bool{}
		for _, l := range undo {
			if l.sha == settingsFile || seen[l.sha] {
				continue
			}
			seen[l.sha] = true
			if item, ok := store.Get(l.sha); ok {
				items = append(items, item)
			} else {
				log.Printf("%s isn't in the data folder any more; left out", l.sha)
				p.unread++
			}
		}
	default:
		return nil, fmt.Errorf("no mode %q", mode)
	}
	slices.SortFunc(items, func(a, b media.Item) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})
	p.files = len(items)
	labels, errs := store.LabelsOnDisk(items)
	for i, item := range items {
		if errs[i] != nil {
			log.Printf("%s: %v; left out", item.Path, errs[i])
			p.unread++
			continue
		}
		after, changed := p.decide(item, labels[i])
		if mode == modeRelabel && media.SameSource(after.Source, media.SourceOther) {
			p.stayOther = append(p.stayOther, item)
		}
		if changed {
			p.changes = append(p.changes, change{item: item, before: labels[i], after: after})
		}
	}
	sources, err := store.HiddenSourcesOnDisk()
	if err != nil {
		return nil, err
	}
	_, p.settings = p.sources(sources)
	return p, nil
}

// apply makes the plan's changes, settings.json first, each in its own turn and decided again from what is on disk
// then, writing each to the journal before it is made. It stops at the first error, with the journal of what it
// changed so far.
func (p *plan) apply(store *media.Store, j *journal) (files int, err error) {
	if _, err := store.EditHiddenSources(func(sources []string) ([]string, error) {
		next, changes := p.sources(sources)
		var lines []journalLine
		for _, c := range changes {
			lines = append(lines, journalLine{settingsFile, fieldHiddenSources, c.old, c.new})
		}
		return next, j.write(lines...)
	}); err != nil {
		return 0, fmt.Errorf("%s: %w", settingsFile, err)
	}
	for _, c := range p.changes {
		changed, err := store.SetLabels(c.item.Sha256(), func(disk media.Labels) (media.Labels, bool, error) {
			next, changed := p.decide(c.item, disk)
			if !changed {
				return disk, false, nil
			}
			return next, true, j.write(labelLines(c.item.Sha256(), disk, next)...)
		})
		if err != nil {
			return files, fmt.Errorf("%s: %w", c.item.Path, err)
		}
		if changed {
			files++
		}
	}
	return files, nil
}

// differ reports whether writing next over disk changes anything a reader sees: the source, the app, or the key
// the app is under.
func differ(disk, next media.Labels) bool {
	return disk.Source != next.Source || disk.ScreenshotApp != next.ScreenshotApp || keyOf(disk) != keyOf(next)
}

// relabelDecider gives each file the rule's source (media.SourceOf, with the user's camera marks and the folder
// kinds counted once for the run, so the dry run and the real one decide by the same counts) and, for a
// screenshot whose name says, its app; an app the rule doesn't give is kept. Ids come as the server writes them
// (folder:WhatsApp for whatsapp), and a rewritten sidecar has its app under screenshotApp.
func relabelDecider(marks media.CameraMarks, kinds *media.FolderKinds) decider {
	return func(item media.Item, disk media.Labels) (media.Labels, bool) {
		source, app := media.SourceOf(item.Sidecar.Facts, marks, kinds)
		next := media.Labels{Source: source, ScreenshotApp: disk.ScreenshotApp}
		if app != nil {
			next.ScreenshotApp = *app
		}
		if next.Source == disk.Source && next.ScreenshotApp == disk.ScreenshotApp {
			return disk, false
		}
		return next, true
	}
}

// relabelSources writes hidden.sources' ids as the server does (whatsapp → folder:WhatsApp).
func relabelSources(sources []string) ([]string, []idChange) {
	return mapSources(sources, func(id string) string { return media.NormalizeSource(id) })
}

// oldIdsDecider writes the ids an older server and app know: folder:WhatsApp as whatsapp, received as camera, and a
// screenshot's app under the key app.
func oldIdsDecider(_ media.Item, disk media.Labels) (media.Labels, bool) {
	next := disk
	switch {
	case media.SameSource(disk.Source, media.SourceWhatsApp):
		next.Source = media.OldSourceWhatsApp
	case media.SameSource(disk.Source, media.SourceReceived):
		next.Source = media.SourceCamera
	}
	if next.ScreenshotApp != "" {
		next.OldAppKey = true
	}
	return next, differ(disk, next)
}

// oldIdsSources writes folder:WhatsApp in hidden.sources as whatsapp. (received is left: as camera it would hide
// every camera photo.)
func oldIdsSources(sources []string) ([]string, []idChange) {
	return mapSources(sources, func(id string) string {
		if media.SameSource(id, media.SourceWhatsApp) {
			return media.OldSourceWhatsApp
		}
		return id
	})
}

// undoDecider puts back what a journal changed in each file, field by field from its last line back, only where
// the field still holds the journal's new value: a later correction stays.
func undoDecider(lines []journalLine) decider {
	bySha := map[string][]journalLine{}
	for _, l := range lines {
		bySha[l.sha] = append(bySha[l.sha], l)
	}
	return func(item media.Item, disk media.Labels) (media.Labels, bool) {
		next := disk
		ls := bySha[item.Sha256()]
		for i := len(ls) - 1; i >= 0; i-- {
			l := ls[i]
			switch l.field {
			case fieldSource:
				if media.SameSource(next.Source, l.new) {
					next.Source = l.old
				}
			case fieldScreenshotApp:
				if next.ScreenshotApp == l.new {
					next.ScreenshotApp = l.old
				}
			case fieldKey:
				if keyOf(next) == l.new {
					next.OldAppKey = l.old == "app"
				}
			}
		}
		return next, differ(disk, next)
	}
}

// undoSources maps back only the ids the journal changed in hidden.sources; ids hidden since stay.
func undoSources(lines []journalLine) sourcesMapper {
	var changes []idChange
	for _, l := range lines {
		if l.sha == settingsFile {
			changes = append(changes, idChange{l.old, l.new})
		}
	}
	return func(sources []string) ([]string, []idChange) {
		return mapSources(sources, func(id string) string {
			for i := len(changes) - 1; i >= 0; i-- {
				if media.SameSource(id, changes[i].new) {
					id = changes[i].old
				}
			}
			return id
		})
	}
}

// mapSources maps each id, with the ids it changed.
func mapSources(sources []string, fn func(string) string) ([]string, []idChange) {
	var changes []idChange
	next := make([]string, len(sources))
	for i, id := range sources {
		next[i] = fn(id)
		if next[i] != id {
			changes = append(changes, idChange{id, next[i]})
		}
	}
	if sources == nil {
		next = nil
	}
	return next, changes
}

// findJournal is the journal at path, or under the data folder's relabels/ when path is only its name.
func findJournal(root, path string) string {
	if _, err := os.Stat(path); err == nil || filepath.IsAbs(path) {
		return path
	}
	for _, p := range []string{filepath.Join(root, path), filepath.Join(root, "relabels", path)} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return path
}

// newJournal is the journal a real run of mode writes for user.
func newJournal(store *media.Store, user, mode string, now func() time.Time) *journal {
	return &journal{root: store.Root(), user: user, mode: mode, now: now}
}
