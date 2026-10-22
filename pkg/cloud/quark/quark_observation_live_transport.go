//go:build quark_live

package quark

// Explicit acceptance-only access to the real transports. Wrappers receive the
// already configured TLS/DNS transport; ordinary product builds expose none of
// these entry points. This does not install a process-global transport.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

func NewObservationValidationClient(c cloud.Config, wrap func(http.RoundTripper, bool) http.RoundTripper) (*Client, error) {
	d, err := NewClient(c)
	if err != nil {
		return nil, err
	}
	d.api.http.Transport = wrap(d.api.http.Transport, false)
	d.mediaTransport = wrap(d.mediaTransport, true)
	return d, nil
}

func NewObservationValidationAuthenticator(c cloud.Config, wrap func(http.RoundTripper, bool) http.RoundTripper) *Authenticator {
	a := NewAuthenticator(c)
	a.api.http.Transport = wrap(a.api.http.Transport, false)
	return a
}

// Validate only the one pre-existing root named by the encrypted ownership
// receipt. No arbitrary object details are exported to the acceptance caller.
func (d *Client) ValidateObservationOwnedRoot(ctx context.Context, ack, name string) error {
	f, err := d.agentInfo(ctx, ack)
	if err != nil {
		return err
	}
	if !f.Dir || f.Parent != "0" || f.Name != name {
		return failure(CodeIdentity)
	}
	return nil
}

func ObservationAgentProfileFingerprint() string {
	// The original authorization helper hashed the optional application platform
	// exactly as configured. Its pinned Agent profile left that field empty;
	// the native API's request-time "web" parameter is not part of that file.
	h := sha256.Sum256([]byte(agentClientID + "\x00" + agentSignKey + "\x00"))
	return hex.EncodeToString(h[:])
}

func ObservationSameAgentHandle(a, b string) bool { return sameAgent(a, b) }
