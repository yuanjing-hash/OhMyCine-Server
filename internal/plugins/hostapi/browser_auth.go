package hostapi

import "context"

type browserAuthenticationKey struct{}
type browserAuthentication struct{ pluginID, connectionID string }

// Only the trusted resource login service may authorize uncredentialed form
// requests to use its pending browser. This is not exposed to WASM guest input.
func WithBrowserAuthentication(ctx context.Context, pluginID, connectionID string) context.Context {
	return context.WithValue(ctx, browserAuthenticationKey{}, browserAuthentication{pluginID, connectionID})
}
func browserAuthenticationAllowed(ctx context.Context, pluginID, connectionID string) bool {
	value, ok := ctx.Value(browserAuthenticationKey{}).(browserAuthentication)
	return ok && value.pluginID == pluginID && value.connectionID == connectionID
}
