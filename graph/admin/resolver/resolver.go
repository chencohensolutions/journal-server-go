package resolver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/enroll"
	"github.com/journal/server-go/graph/admin/model"
)

// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require
// here.

type Resolver struct {
	Config     auth.AppConfig
	Enrollment *enroll.Service
	Now        func() time.Time
	// Reads what journal-server index added to the indexes' caches; nil when the server reads files itself.
	RefreshIndexes func()
	// The copies of the users' data folders (docs/copies.md), and how a full pass of one is run (and logged).
	Copies  []*copies.Copier
	RunCopy func(*copies.Copier)
}

// copiersOf are the copies of user, or of every user when user is nil.
func (r *Resolver) copiersOf(user *string) []*copies.Copier {
	var found []*copies.Copier
	for _, c := range r.Copies {
		if user == nil || c.User == *user {
			found = append(found, c)
		}
	}
	return found
}

// copyReport is how c is doing; with check, also what it lacks now.
func copyReport(ctx context.Context, c *copies.Copier, check, paths bool) *model.CopyReport {
	report := &model.CopyReport{User: c.User, Folder: c.Folder, At: c.At, Realtime: c.Realtime}
	report.SameDisk, _ = c.SameDisk()
	running, last := c.Status()
	if running != nil {
		report.Running = &model.CopyProgress{StartedAt: running.StartedAt, Files: running.Files, Bytes: float64(running.Bytes)}
	}
	if last != nil {
		report.LastPass = &model.CopyPass{StartedAt: last.StartedAt, FinishedAt: last.FinishedAt, Files: last.Files,
			Bytes: float64(last.Bytes), Failed: last.Failed, Error: errorText(last.Err)}
	}
	if !check {
		return report
	}
	lacks, err := c.Check(ctx, paths)
	if err != nil {
		report.Error = errorText(err)
		return report
	}
	report.Originals, report.OriginalBytes = lacks.Originals, float64(lacks.OriginalBytes)
	report.Sidecars, report.InCopyOnly = lacks.Sidecars, lacks.InCopyOnly
	if paths {
		report.Paths = lacks.Paths
		if report.Paths == nil {
			report.Paths = []string{}
		}
	}
	return report
}

func errorText(err error) *string {
	if err == nil {
		return nil
	}
	text := err.Error()
	return &text
}

func noCopies(user *string) error {
	if user == nil {
		return errors.New("no user has copies in config.json")
	}
	return fmt.Errorf("user %q has no copies in config.json", *user)
}
