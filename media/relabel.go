package media

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
)

// Labels are a file's source and its screenshot's app as its sidecar has them on disk: not as the store holds them
// in memory, where an old id is already read as the new one (normalizeFacts). scripts/fix-sources relabels files,
// undoes a relabel and writes the ids an older server knows by them, so it compares and writes what is on disk.
type Labels struct {
	Source        string // "" when the sidecar has none
	ScreenshotApp string // "" when it has none
	// The app is written under the key it had before Oct 27, 2026, "app", which an older server reads, rather than
	// "screenshotApp". Only meaningful with a ScreenshotApp.
	OldAppKey bool
}

// readDiskSidecar reads a sidecar from disk, with its labels as they are there.
func readDiskSidecar(path string) (Sidecar, Labels, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sidecar{}, Labels{}, err
	}
	var sidecar Sidecar
	var keys struct {
		Facts struct {
			Source        *string `json:"source"`
			ScreenshotApp *string `json:"screenshotApp"`
			App           *string `json:"app"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(data, &sidecar); err != nil {
		return Sidecar{}, Labels{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(data, &keys); err != nil {
		return Sidecar{}, Labels{}, fmt.Errorf("%s: %w", path, err)
	}
	var labels Labels
	if s := keys.Facts.Source; s != nil {
		labels.Source = *s
	}
	switch {
	case keys.Facts.ScreenshotApp != nil:
		labels.ScreenshotApp = *keys.Facts.ScreenshotApp
	case keys.Facts.App != nil:
		labels.ScreenshotApp, labels.OldAppKey = *keys.Facts.App, true
	}
	return sidecar, labels, nil
}

// LabelsOnDisk reads the labels of each item from its sidecar on disk, several at once (as the start-up scan reads
// them), with an error for each sidecar that can't be read. It only reads.
func (s *Store) LabelsOnDisk(items []Item) ([]Labels, []error) {
	labels := make([]Labels, len(items))
	errs := make([]error, len(items))
	parallel(len(items), scanReaders, func(i int) {
		_, labels[i], errs[i] = readDiskSidecar(s.sidecarPath(&items[i]))
	})
	return labels, errs
}

// oldKeySidecar is a sidecar written with its screenshot's app under the old key, "app" (Labels.OldAppKey).
type oldKeySidecar struct {
	Sidecar
	Facts oldKeyFacts `json:"facts"`
}

type oldKeyFacts struct {
	Facts
	// Hides Facts.ScreenshotApp (a field nearer the top wins in encoding/json); always nil.
	ScreenshotApp *string `json:"screenshotApp,omitempty"`
	App           *string `json:"app,omitempty"`
}

// writeSidecarKeys writes a sidecar to path, whole or not at all, with the screenshot's app under the old key when
// oldAppKey.
func writeSidecarKeys(path string, sidecar Sidecar, oldAppKey bool) error {
	if !oldAppKey || sidecar.Facts.ScreenshotApp == nil {
		return writeSidecar(path, sidecar)
	}
	data, err := json.MarshalIndent(oldKeySidecar{Sidecar: sidecar, Facts: oldKeyFacts{Facts: sidecar.Facts, App: sidecar.Facts.ScreenshotApp}}, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// editOnDisk rewrites a file's sidecar, inside a turn, as edit changes the one on disk (read again in the turn), so
// that only what edit changes changes: a source id stays as it is on disk (not read as the new one) and the
// screenshot's app stays under its key, unless edit changes labels. edit returns whether to write at all.
func (s *Store) editOnDisk(sha string, edit func(sidecar *Sidecar, labels *Labels) (bool, error)) (changed bool, err error) {
	err = s.Turn(func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		item, ok := s.items[sha]
		if !ok {
			return fmt.Errorf("%s isn't in the data folder", sha)
		}
		path := s.sidecarPath(item)
		sidecar, labels, err := readDiskSidecar(path)
		if err != nil {
			return err
		}
		if sidecar.Sha256 != sha {
			return fmt.Errorf("%s has sha256 %s, not %s", path, sidecar.Sha256, sha)
		}
		write, err := edit(&sidecar, &labels)
		if err != nil || !write {
			return err
		}
		if err := writeSidecarKeys(path, sidecar, labels.OldAppKey); err != nil {
			return err
		}
		changed = true
		item.Sidecar = sidecar
		s.indexItem(item)
		s.wrote(item)
		return nil
	})
	return changed, err
}

// SetLabels rewrites a file's source and its screenshot's app in its sidecar, in one turn, as change decides from
// the labels on disk (read again in the turn, so a file changed since they were last read is decided by what it has
// now). change returns the labels to write and whether to write them; it runs before the sidecar is written, so it
// can note the change elsewhere first (scripts/fix-sources' journal). The labels are written as given, not
// normalized, so the ids an older server knows can be written back (the store's memory reads them as the new ones,
// as always). SentFrom keeps the source the file arrived with (keepSentFrom), and when the source or the app changes
// CorrectedAt is set, so devices get the file again in the change feed. Notes, nudity and privacy stay as they are;
// the original is never touched.
func (s *Store) SetLabels(sha string, change func(disk Labels) (Labels, bool, error)) (changed bool, err error) {
	return s.editOnDisk(sha, func(sidecar *Sidecar, labels *Labels) (bool, error) {
		next, write, err := change(*labels)
		if err != nil || !write || next == *labels {
			return false, err
		}
		facts := &sidecar.Facts
		if next.Source != labels.Source || next.ScreenshotApp != labels.ScreenshotApp {
			now := s.now().UTC()
			facts.CorrectedAt = &now
		}
		keepSentFrom(facts)
		facts.Source = optional(next.Source)
		facts.ScreenshotApp = optional(next.ScreenshotApp)
		*labels = next
		return true, nil
	})
}

// SetCameraFact writes the camera a file's tags name into its sidecar (facts.camera), in a turn, changing nothing
// else in it (its source as it is on disk, the screenshot's app under its key) and leaving CorrectedAt as it was: a
// file's camera is shown in its info, read when it is shown. A file that already has that camera is left as it is.
func (s *Store) SetCameraFact(sha string, camera Camera) (changed bool, err error) {
	return s.editOnDisk(sha, func(sidecar *Sidecar, _ *Labels) (bool, error) {
		if c := sidecar.Facts.Camera; c != nil && *c == camera {
			return false, nil
		}
		sidecar.Facts.Camera = &camera
		return true, nil
	})
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// HiddenSourcesOnDisk is settings.json's hidden.sources as written there (not normalized); nil without a
// settings.json or sources.
func (s *Store) HiddenSourcesOnDisk() ([]string, error) {
	_, _, sources, err := s.readSettingsRaw()
	return sources, err
}

// readSettingsRaw reads settings.json as it is: its keys, its hidden's keys, and hidden.sources. Nil maps without
// the file.
func (s *Store) readSettingsRaw() (file, hidden map[string]json.RawMessage, sources []string, err error) {
	data, err := os.ReadFile(s.settingsPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, nil, fmt.Errorf("%s: %w", s.settingsPath(), err)
	}
	if raw, ok := file["hidden"]; ok {
		if err := json.Unmarshal(raw, &hidden); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.settingsPath(), err)
		}
	}
	if raw, ok := hidden["sources"]; ok {
		if err := json.Unmarshal(raw, &sources); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", s.settingsPath(), err)
		}
	}
	return file, hidden, sources, nil
}

// EditHiddenSources rewrites settings.json's hidden.sources, in a turn, as edit returns them from the ones written
// there now (read again in the turn). They are written as given, not normalized, so an older server's ids can be
// written back, and nothing else in the file changes. edit runs before the file is written, so it can note the
// change elsewhere first. Nothing is written without a settings.json, or when edit returns them as they were. The
// store reads what is hidden again; another process (a running server) reads settings.json when it starts.
func (s *Store) EditHiddenSources(edit func(sources []string) ([]string, error)) (changed bool, err error) {
	err = s.Turn(func() error {
		file, hidden, sources, err := s.readSettingsRaw()
		if err != nil || file == nil {
			return err
		}
		next, err := edit(slices.Clone(sources))
		if err != nil || slices.Equal(next, sources) {
			return err
		}
		if hidden == nil {
			hidden = map[string]json.RawMessage{}
		}
		if hidden["sources"], err = json.Marshal(next); err != nil {
			return err
		}
		if file["hidden"], err = json.Marshal(hidden); err != nil {
			return err
		}
		data, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.settingsPath(), append(data, '\n')); err != nil {
			return err
		}
		changed = true
		h, err := s.readHidden()
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.hidden = h
		s.mu.Unlock()
		return nil
	})
	return changed, err
}
