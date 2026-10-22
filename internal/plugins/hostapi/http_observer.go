package hostapi

import "context"

type httpMethodObserverKey struct{}

// WithHTTPMethodObserver lets a Host-owned invocation prohibit unsafe replay.
// Guest input cannot create the context value. The observer receives only the
// canonical GET/POST method, never a URL, body, header or credential.
func WithHTTPMethodObserver(ctx context.Context, observer func(string)) context.Context {
	return context.WithValue(ctx, httpMethodObserverKey{}, observer)
}

func observeHTTPMethod(ctx context.Context, method string) {
	if observer, ok := ctx.Value(httpMethodObserverKey{}).(func(string)); ok && observer != nil {
		observer(method)
	}
}
