package resolver

import (
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/enroll"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Config     auth.AppConfig
	Enrollment *enroll.Service
	Now        func() time.Time
}
