package downloader

import "context"

type selectionFenceContext struct{}

// WithSelectionFence carries a local authority check, never wire data. Each
// executor rebinds its own durable epoch/lease after an authenticated RPC.
func WithSelectionFence(ctx context.Context, check func() error) context.Context {
	return context.WithValue(ctx, selectionFenceContext{}, check)
}
func CheckSelectionFence(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if check, ok := ctx.Value(selectionFenceContext{}).(func() error); ok {
		return check()
	}
	return nil
}
