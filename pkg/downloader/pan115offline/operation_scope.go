package pan115offline

import (
	"context"
	"strings"

	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

func (c *Client) BindTaskOperation(scope string) error {
	scope = strings.TrimPrefix(strings.TrimSpace(scope), "omc-")
	if !validTaskTag("omc-" + scope) {
		return downloader.Error("downloader_source_invalid", false, nil)
	}
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if c.operationScope != "" && c.operationScope != scope {
		return downloader.Error("downloader_operation_scope_conflict", false, nil)
	}
	c.operationScope = scope
	return nil
}
func (c *Client) taskOperationContext(ctx context.Context, tag string) (context.Context, error) {
	c.scopeMu.Lock()
	scope := c.operationScope
	c.scopeMu.Unlock()
	if tag != "" {
		given := strings.TrimPrefix(tag, "omc-")
		if scope != "" && scope != given {
			return ctx, downloader.Error("downloader_operation_scope_conflict", false, nil)
		}
		if scope == "" {
			scope = given
		}
	}
	if scope != "" {
		ctx = cloud.WithOfflineOperationScope(ctx, "download:"+scope)
	}
	return ctx, nil
}

var _ downloader.TaskOperationBinder = (*Client)(nil)
