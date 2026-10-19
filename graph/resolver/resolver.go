package resolver

import (
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/media"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Enrollment *enroll.Service
	Media      *media.Library
}
