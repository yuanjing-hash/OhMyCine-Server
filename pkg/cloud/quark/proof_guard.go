package quark

import "context"

type commonSpaceGuardKey struct{}

// WithCommonSpaceGuard binds one proof's I/O, persistence and owned cleanup to
// the Server's current configuration and route identities. Same-identity token
// rotation is allowed; replacing authorization cannot publish an old proof.
func WithCommonSpaceGuard(ctx context.Context, guard func(context.Context) error) context.Context {
	return context.WithValue(ctx, commonSpaceGuardKey{}, guard)
}

func checkCommonSpaceGuard(ctx context.Context) error {
	if guard, ok := ctx.Value(commonSpaceGuardKey{}).(func(context.Context) error); ok && guard != nil {
		return guard(ctx)
	}
	return nil
}
