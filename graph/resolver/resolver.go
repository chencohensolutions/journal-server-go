package resolver

import (
	"github.com/journal/server-go/aesthetic"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/ocr"
	"github.com/journal/server-go/search"
	"github.com/journal/server-go/similar"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Enrollment *enroll.Service
	Library    *media.Library
	// Text search; nil when it is off.
	Search *search.Service
	// Face search; nil when it is off.
	Faces *faces.Service
	// Text in photos (OCR), similar photos and duplicates (DINOv2), aesthetic scores (LAION); each nil when off.
	// Named so as not to clash with gqlgen's resolvers of Media.text and Media.aesthetic.
	TextInPhotos    *ocr.Service
	SimilarPhotos   *similar.Service
	AestheticScores *aesthetic.Service
	// New files are scanned for nudity (package nudity), for serverInfo.
	NudityScan bool
	// Previews and posters, for serverInfo to say whether they can be made.
	Previews *media.Previews
	// The server's build, shown by serverInfo.
	Version string
}

// APIVersion is serverInfo.api: raised when a change needs newer apps.
const APIVersion = 1
