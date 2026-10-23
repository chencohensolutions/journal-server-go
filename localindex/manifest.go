package localindex

import (
	"errors"

	"github.com/journal/server-go/media"
)

// Index is what the manifest tells a phone about an index it may keep.
type Index struct {
	Name   string
	Model  string
	Format string
	Dim    int
	// Files is how many of the files the device may see have been read for the index; Bytes about how much a
	// stream from the start sends (the cache file's records, in proportion).
	Files int
	Bytes int64
	// Models is the files a phone needs to run the index (whose hash is known); none for an index of data only.
	Models []ModelFile
}

// Manifest is the indexes user's phone may keep, as the device of view sees them, in the order they were given.
func (s *Server) Manifest(user string, view media.View) []Index {
	if !s.On() {
		return nil
	}
	store := s.library.Store(user)
	if store == nil {
		return nil
	}
	var items []media.Item
	store.Range(func(item media.Item) { items = append(items, item) })
	hidden := make([]bool, len(items))
	for i, item := range items {
		hidden[i] = store.Hides(view, item)
	}
	var out []Index
	for _, name := range s.order {
		src := s.sources[name]
		x := Index{Name: name, Model: src.Cache.Model(), Format: src.Format, Models: s.models.of(name)}
		read := 0
		for i, item := range items {
			if !src.Cache.Has(user, item.Sha256()) {
				continue
			}
			read++
			if !hidden[i] {
				x.Files++
			}
		}
		file, err := openCache(src.Cache.CacheFile(user), src.Format)
		switch {
		case err == nil:
			x.Dim = file.dim
			if read > 0 {
				x.Bytes = (file.size - file.start) * int64(x.Files) / int64(read)
			}
			file.Close()
		case !errors.Is(err, errNoHeader):
			continue
		}
		out = append(out, x)
	}
	return out
}
