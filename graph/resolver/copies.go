package resolver

import (
	"context"
	"errors"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/graph/model"
	"github.com/journal/server-go/media"
)

// CopyRunner gives the copies fields each user's copies (copy.go in the server): passes and compares it starts run
// in the background, past the request.
type CopyRunner interface {
	Of(user string) []*copies.Copier
	// Start runs a full pass now, unless one is going.
	Start(c *copies.Copier)
	// CompareIfOld compares the copy with the data folder again when the last compare is old.
	CompareIfOld(c *copies.Copier)
}

// copiesOf are the request's user's copies; none on a server without copies.
func (r *Resolver) copiesOf(ctx context.Context) ([]*copies.Copier, error) {
	if _, err := r.store(ctx); err != nil {
		return nil, err
	}
	if r.Copies == nil {
		return nil, nil
	}
	return r.Copies.Of(auth.FromContext(ctx).Username), nil
}

// dataCopy is how a copy is doing, from its status and its last compare; an old compare is made again meanwhile.
func (r *Resolver) dataCopy(c *copies.Copier) *model.DataCopy {
	r.Copies.CompareIfOld(c)
	result := &model.DataCopy{Folder: c.Folder, Realtime: c.Realtime, DailyAt: c.At}
	result.SameDisk, _ = c.SameDisk()
	running, last := c.Status()
	if running != nil {
		result.Running = &model.CopyProgress{StartedAt: running.StartedAt, Files: running.Files, Bytes: running.Bytes}
	}
	if last != nil {
		result.LastPass = &model.CopyPass{StartedAt: last.StartedAt, FinishedAt: last.FinishedAt, Files: last.Files,
			Bytes: last.Bytes, Failed: last.Failed}
	}
	compared, comparing := c.LastCompare()
	result.Checking = comparing
	if compared != nil && !compared.At.IsZero() {
		at := compared.At
		result.CheckedAt = &at
		result.Originals = compared.Total
		result.Copied = compared.Total - compared.Originals
		result.Missing, result.MissingBytes = compared.Originals, compared.OriginalBytes
		result.InCopyOnly = compared.InCopyOnly
	}
	if since := c.UnreachableSince(); !since.IsZero() {
		problem := model.CopyProblemFolderMissing
		result.Problem, result.ProblemSince = &problem, &since
	} else if last != nil && errors.Is(last.Err, copies.ErrDiskFull) {
		problem := model.CopyProblemDiskFull
		at := last.FinishedAt
		result.Problem, result.ProblemSince = &problem, &at
	}
	if result.Problem == nil || *result.Problem != model.CopyProblemFolderMissing {
		if free, err := media.FreeBytes(c.Folder); err == nil {
			bytes := int64(free)
			result.FreeBytes = &bytes
		}
	}
	return result
}

