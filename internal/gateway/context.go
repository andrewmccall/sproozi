package gateway

import "context"

type identityContextKey struct{}

func WithIdentity(ctx context.Context, identity *ExecutionGrant) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

func IdentityFromContext(ctx context.Context) (*ExecutionGrant, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(*ExecutionGrant)
	return identity, ok && identity != nil
}
