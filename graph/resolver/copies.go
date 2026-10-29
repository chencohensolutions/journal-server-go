package resolver

import (
	"context"
	"errors"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/graph/model"
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

// copyOf is the request's user's copy in folder.
func (r *Resolver) copyOf(ctx context.Context, folder string) (*copies.Copier, error) {
	mine, err := r.copiesOf(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range mine {
		if c.Folder == folder {
			return c, nil
		}
	}
	return nil, coded(CodeInvalidArgument, "%q is not one of your copies", folder)
}

// dataCopy is how a copy is doing, from its status and its last compare; an old compare is made again meanwhile.
func (r *Resolver) dataCopy(c *copies.Copier) *model.DataCopy {
	r.Copies.CompareIfOld(c)
	result := &model.DataCopy{Folder: c.Folder, Kind: model.CopyKindFolder, Realtime: c.Realtime, DailyAt: c.At,
		Originals: c.Originals()}
	missing := model.CopyProblemFolderMissing
	if c.PeerName() != "" {
		result.Kind, missing = model.CopyKindPeer, model.CopyProblemPeerUnreachable
	}
	result.SameDisk, _ = c.SameDisk()
	running, last := c.Status()
	if running != nil {
		result.Running = &model.CopyProgress{StartedAt: running.StartedAt, Files: running.Files, Bytes: running.Bytes,
			Counting: running.Counting, TotalFiles: running.TotalFiles, TotalBytes: running.TotalBytes,
			DoneFiles: running.DoneFiles, DoneBytes: running.DoneBytes}
	}
	if last != nil {
		result.LastPass = &model.CopyPass{StartedAt: last.StartedAt, FinishedAt: last.FinishedAt, Files: last.Files,
			Bytes: last.Bytes, Failed: last.Failed, Stopped: last.Stopped}
	}
	compared, comparing := c.LastCompare()
	result.Checking = comparing
	if compared != nil && !compared.At.IsZero() {
		at := compared.At
		result.CheckedAt = &at
		result.Copied = compared.Total - compared.Originals
		result.Missing, result.MissingBytes = compared.Originals, compared.OriginalBytes
		result.InCopyOnly = compared.InCopyOnly
	}
	if since := c.UnreachableSince(); !since.IsZero() {
		result.Problem, result.ProblemSince = &missing, &since
	} else if last != nil && (errors.Is(last.Err, copies.ErrDiskFull) || errors.Is(last.Err, copies.ErrQuotaFull)) {
		problem := model.CopyProblemDiskFull
		if errors.Is(last.Err, copies.ErrQuotaFull) {
			problem = model.CopyProblemQuotaFull
		}
		at := last.FinishedAt
		result.Problem, result.ProblemSince = &problem, &at
	}
	if result.Problem == nil || *result.Problem != missing {
		if free, err := c.FreeBytes(); err == nil {
			bytes := int64(free)
			result.FreeBytes = &bytes
		}
	}
	return result
}

