package auth

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
)

// PublicDirective marks root fields that anonymous clients (no client certificate) may call.
const PublicDirective = "public"

// RequireUserUnlessPublic is a gqlgen root field middleware: anonymous requests may only reach
// root fields marked @public. Everything else, including introspection, needs a client certificate,
// so a new field is protected unless it opts out.
func RequireUserUnlessPublic(ctx context.Context, next graphql.RootResolver) graphql.Marshaler {
	if FromContext(ctx) != nil {
		return next(ctx)
	}
	field := graphql.GetRootFieldContext(ctx).Field
	if field.Definition != nil && field.Definition.Directives.ForName(PublicDirective) != nil {
		return next(ctx)
	}
	graphql.AddErrorf(ctx, "client certificate required for %s", field.Name)
	return graphql.Null
}

// PassThroughDirective implements @public at field level; access is decided by RequireUserUnlessPublic.
func PassThroughDirective(ctx context.Context, _ any, next graphql.Resolver) (any, error) {
	return next(ctx)
}
