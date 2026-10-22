package resolver

import (
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/media"
	"github.com/journal/server-go/search"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Enrollment *enroll.Service
	Media      *media.Library
	// Text search; nil when it is off.
	Search *search.Service
	// Previews and posters, for serverInfo to say whether they can be made.
	Previews *media.Previews
	// The server's build, shown by serverInfo.
	Version string
}

// APIVersion is serverInfo.api: raised when a change needs newer apps.
const APIVersion = 1
