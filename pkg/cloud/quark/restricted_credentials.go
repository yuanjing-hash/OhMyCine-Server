package quark

import (
	"context"
	"encoding/json"
	"reflect"
)

// SetRestrictedCredentialLoader is an explicit access-grant seam for Node. It
// runs before authenticated requests are constructed, never after a request has
// already captured its token. Normal Server renewal and credential CAS are
// unaffected. The caller remains responsible for checking the task, action,
// lease and expiry on every use; the driver enforces immutable cloud scope.
func (d *Client) SetRestrictedCredentialLoader(load func(context.Context) (json.RawMessage, error)) error {
	d.restrictedMu.Lock()
	defer d.restrictedMu.Unlock()
	if load == nil || d.restrictedLoader != nil || d.config.PersistCredentials != nil || !restrictedCredentials(d.Credentials()) {
		return failure(CodeIdentity)
	}
	d.restrictedLoader = load
	return nil
}

func restrictedCredentials(c Credentials) bool {
	if c.Web != nil {
		return false
	}
	for _, a := range []*Authorization{c.TV, c.Agent} {
		if a != nil && (a.RefreshToken != "" || !a.IdentityVerified) {
			return false
		}
	}
	return c.TV != nil || c.Agent != nil
}

func (d *Client) loadRestrictedCredentials(ctx context.Context) error {
	d.restrictedMu.Lock()
	defer d.restrictedMu.Unlock()
	if d.restrictedLoader == nil {
		return nil
	}
	raw, err := d.restrictedLoader(ctx)
	if err != nil {
		return err
	}
	next, err := ParseCredentials(raw)
	if err != nil || !restrictedCredentials(next) {
		return failure(CodeIdentity)
	}
	current := d.Credentials()
	if next.Version != current.Version || next.Revision < current.Revision || next.Revision == current.Revision && !reflect.DeepEqual(next, current) || !reflect.DeepEqual(next.Proofs, current.Proofs) || !sameRestrictedRoute(current.TV, next.TV) || !sameRestrictedRoute(current.Agent, next.Agent) {
		return failure(CodeIdentity)
	}
	d.mu.Lock()
	d.credentials = cloneCredentials(next)
	d.mu.Unlock()
	return nil
}

func sameRestrictedRoute(old, next *Authorization) bool {
	if old == nil || next == nil {
		return old == nil && next == nil
	}
	return old.AccountID == next.AccountID && old.DeviceID == next.DeviceID && old.ClientDeviceID == next.ClientDeviceID && old.IdentityVerified == next.IdentityVerified && next.Generation >= old.Generation
}
