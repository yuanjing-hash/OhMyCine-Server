package pan115

import (
	"context"
	"errors"

	pan115sdk "github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
)

// Quota uses the existing account-space endpoint without running a directory
// scan or the multi-request connection health test.
func (c *Client) Quota(ctx context.Context) (cloud.Quota, error) {
	var info pan115sdk.InfoData
	err := c.waitReadAndCall(ctx, c.readLimiter(ctx), func() error {
		var err error
		info, err = c.sdk.GetInfo()
		return err
	})
	if err != nil {
		return cloud.Quota{}, mapError(err)
	}
	used, total := info.SpaceInfo.AllUse.Size, info.SpaceInfo.AllTotal.Size
	if used < 0 || total <= 0 || used > total {
		return cloud.Quota{}, cloud.Error(cloud.CodeResponseInvalid, false, errors.New("invalid quota measurement"))
	}
	return cloud.Quota{UsedBytes: uint64(used), TotalBytes: uint64(total)}, nil
}
