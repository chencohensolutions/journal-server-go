// Package media keeps a user's originals in their data folder: each file byte for byte under
// media/YYYY/YYYY-MM/, with a JSON sidecar next to it. The files are the truth; the index in memory
// is rebuilt from the sidecars whenever a store opens. See docs/media.md.
package media

import (
	"encoding/json"
	"time"
)

// SidecarFormat is the version of the sidecar layout written by this server.
const SidecarFormat = 1

type Kind string

const (
	KindPhoto Kind = "photo"
	KindVideo Kind = "video"
)

type GeoPoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Facts describe the file. The device sends them with the upload (it already read them from
// MediaStore/PhotoKit); the server adds who uploaded it and when.
type Facts struct {
	FileName   string     `json:"fileName"`
	Kind       Kind       `json:"kind"`
	MimeType   string     `json:"mimeType"`
	Size       int64      `json:"size"`
	TakenAt    *time.Time `json:"takenAt"`
	Width      *int       `json:"width"`
	Height     *int       `json:"height"`
	DurationMs *int64     `json:"durationMs"`
	Location   *GeoPoint  `json:"location"`
	Source     *string    `json:"source"`
	// The photo this file is an edit of.
	VersionOf *string `json:"versionOf"`
	// The file this one belongs with: a Live Photo's video names its photo, a RAW its JPEG.
	PairedWith *string   `json:"pairedWith"`
	UploadedAt time.Time `json:"uploadedAt"`
	UploadedBy string    `json:"uploadedBy"`
}

// Sidecar is the <file>.json next to an original.
type Sidecar struct {
	Format int    `json:"format"`
	Sha256 string `json:"sha256"`
	Facts  Facts  `json:"facts"`
	// Caption and tags, written through sync (not built yet). Kept as is when the facts are rewritten.
	Note json.RawMessage `json:"note,omitempty"`
}

// Item is one original known to a store.
type Item struct {
	// Path of the original relative to the data folder, with forward slashes.
	Path    string
	Sidecar Sidecar
}

func (i Item) Sha256() string { return i.Sidecar.Sha256 }
