package gateway

import "context"

type identityContextKey struct{}

func WithIdentity(ctx context.Context, identity *RunIdentity) context.Context {
	return context.WithValue(ctx, identityContextKey{}, identity)
}

func IdentityFromContext(ctx context.Context) (*RunIdentity, bool) {
	identity, ok := ctx.Value(identityContextKey{}).(*RunIdentity)
	return identity, ok && identity != nil
}
