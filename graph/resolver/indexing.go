package resolver

import (
	"context"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/graph/model"
)

// IndexRunner runs the reading of each user's files for the indexes (indexrun.go in the server), for the indexing
// fields. Each call is about one user's own run.
type IndexRunner interface {
	Status(user string) *model.Indexing
	Start(user string) (*model.Indexing, error)
	Pause(user string) (*model.Indexing, error)
}

// indexingUser is the request's user, for the indexing fields; an error when no run can be started here.
func (r *Resolver) indexingUser(ctx context.Context) (string, error) {
	if _, err := r.store(ctx); err != nil {
		return "", err
	}
	if r.IndexRuns == nil {
		return "", coded(CodeIndexingOff, "this server reads no files for indexes (no model or runtime)")
	}
	return auth.FromContext(ctx).Username, nil
}
