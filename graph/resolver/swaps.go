package resolver

import (
	"context"
	"time"

	"github.com/journal/server-go/auth"
	"github.com/journal/server-go/copies"
	"github.com/journal/server-go/graph/model"
)

// SwapService sets up each user's swaps from their phone (swap.go in the server). Every method acts on the user
// named only: one user's friends, copies and invites are never another's.
type SwapService interface {
	// Info is serverInfo.swap for user.
	Info(user string) *model.SwapInfo
	Invite(user, name string, quota int64) (*model.SwapInvite, error)
	// Join keeps user's copy at the server of a friend's invite; a refusal is the user's to act on.
	Join(ctx context.Context, user string, join SwapJoin) (JoinOutcome, error)
	// Accept joins the swap offered back with id.
	Accept(ctx context.Context, user, id string, passphrase *string, plain bool) (JoinOutcome, error)
	Decline(user, id string) bool
	Mine(user string) []Swap
	Kept(user string) []*model.KeptSwap
	Offers(user string) []*model.SwapOffer
	Remove(user, name string) error
	Revoke(user, name string, deleteFiles bool) error
}

// SwapJoin is what joinSwap asks.
type SwapJoin struct {
	Link, Name string
	Passphrase *string
	Plain      bool
	OfferQuota int64
}

// JoinOutcome is a swap joined (Swap, and whether a copy here was offered back) or Refused.
type JoinOutcome struct {
	Swap    *Swap
	Offered bool
	Refused *model.SwapRefused
}

// Swap is user's copy at a friend's server.
type Swap struct {
	Name      string
	Quota     int64
	AddedAt   time.Time
	Encrypted bool
	// The copy; nil when it couldn't be opened.
	Copier *copies.Copier
}

// swapUser is the request's user, with a data folder.
func (r *Resolver) swapUser(ctx context.Context) (string, error) {
	if _, err := r.store(ctx); err != nil {
		return "", err
	}
	if r.SwapSetup == nil {
		return "", coded(CodeInvalidArgument, "this server sets up no swaps")
	}
	return auth.FromContext(ctx).Username, nil
}

func (r *Resolver) mySwap(s Swap) *model.MySwap {
	out := &model.MySwap{Name: s.Name, Quota: s.Quota, AddedAt: s.AddedAt, Encrypted: s.Encrypted}
	if s.Copier != nil && r.Copies != nil {
		out.Copy = r.dataCopy(s.Copier)
	}
	return out
}

func (r *Resolver) joinResult(o JoinOutcome) model.JoinSwapResult {
	if o.Refused != nil {
		return o.Refused
	}
	return &model.SwapJoined{Swap: r.mySwap(*o.Swap), Offered: o.Offered}
}
