//go:build quark_live

package quark

import (
	"context"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// ObservationListAgentDirectory is acceptance-only metadata access. The caller
// supplies the existing bounded transport; this method shares its account,
// proof, pagination, deadline and request limits and exposes no credentials.
func (d *Client) ObservationListAgentDirectory(ctx context.Context, parent string) ([]cloud.Item, error) {
	a, err := d.authorization(ctx, RouteAgent)
	if err != nil {
		return nil, err
	}
	id, err := parseID(parent, RouteAgent, a.AccountID, false)
	if err != nil {
		return nil, err
	}
	dir, err := d.managed(ctx, parent)
	if err != nil {
		return nil, err
	}
	if !dir.Dir {
		return nil, failure(CodeIdentity)
	}
	items, err := d.agentDirectory(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.Item, 0, len(items))
	for _, item := range items {
		out = append(out, toCloudAgent(a.AccountID, item))
	}
	return out, nil
}
