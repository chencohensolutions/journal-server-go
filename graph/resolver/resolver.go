package resolver

import (
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/faces"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search"
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
	// New files are scanned for nudity (package nudity), for serverInfo.
	NudityScan bool
	// Previews and posters, for serverInfo to say whether they can be made.
	Previews *media.Previews
	// The server's build, shown by serverInfo.
	Version string
}

// APIVersion is serverInfo.api: raised when a change needs newer apps.
const APIVersion = 1
