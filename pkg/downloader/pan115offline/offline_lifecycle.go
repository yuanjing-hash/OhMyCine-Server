package pan115offline

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"time"
)

func (c *Client) OfflineOperation(source downloader.Source) downloader.OfflineOperation {
	if source.Kind != downloader.SourceURL && source.Kind != downloader.SourceTorrent {
		return downloader.OfflineOperation{}
	}
	// A raw torrent needs the actual native-wanted adapter, not a share route.
	if source.Kind == downloader.SourceTorrent && source.ContentSelection == nil {
		return downloader.OfflineOperation{}
	}
	_, proof := c.driver.(cloud.FinalizedFileReader)
	return downloader.OfflineOperation{Kind: downloader.OfflineOperationCloud, AccountNamespace: c.accountNamespace, KeepOutputStop: true, RequiredFileCompletion: proof}
}
func (c *Client) BindOfflineRequired(plan downloader.OfflineRequiredPlan) error {
	digest, err := plan.Digest()
	if err != nil {
		return err
	}
	c.scopeMu.Lock()
	defer c.scopeMu.Unlock()
	if c.required != nil {
		previous, _ := c.required.Digest()
		if previous != digest {
			return downloader.Error("downloader_offline_required_conflict", false, nil)
		}
		return nil
	}
	copy := plan
	copy.Files = append([]downloader.OfflineRequiredFile(nil), plan.Files...)
	c.required = &copy
	return nil
}
func (c *Client) observeOffline(ctx context.Context, native cloud.OfflineTask) downloader.Task {
	task := mapTask(native)
	task.Offline.AccountNamespace = c.accountNamespace
	if task.Offline.StartProvenance == downloader.OfflineStartProviderAdded {
		c.scopeMu.Lock()
		scope := c.operationScope
		c.scopeMu.Unlock()
		owned := false
		if scope != "" && native.DirectoryID != "" {
			root, e := c.driver.Stat(ctx, native.DirectoryID)
			owned = e == nil && root.ID == native.DirectoryID && root.ParentID == c.directoryID && root.Name == "omc-"+scope && root.IsDir
		}
		if !owned {
			task.Offline.StartedAt = nil
			task.Offline.StartProvenance = ""
		}
	}
	c.scopeMu.Lock()
	plan := c.required
	c.scopeMu.Unlock()
	if plan == nil || task.Failed || native.OutputItemID == "" {
		return task
	}
	digest, _ := plan.Digest()
	task.Offline.RequiredDigest = digest
	if native.Completed {
		return task
	}
	// Optional evidence has a finite metadata budget. Failure/unsupported schema
	// is unknown proof and never turns a usable aggregate route into a failure.
	proofCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	manifest, ok := c.requiredReady(proofCtx, native.OutputItemID, *plan)
	if ok {
		now := time.Now().UTC()
		task.Offline.Ready = true
		task.Offline.StopPending = true
		task.Offline.ReadyAt = &now
		task.Offline.CompletionMode = downloader.OfflineCompletionRequiredFiles
		task.Offline.ReadyManifest = &manifest
	}
	return task
}
func (c *Client) requiredReady(ctx context.Context, outputID string, plan downloader.OfflineRequiredPlan) (downloader.Manifest, bool) {
	_, ok := c.driver.(cloud.FinalizedFileReader)
	if !ok || c.accountNamespace == "" {
		return downloader.Manifest{}, false
	}
	// Revalidate the exact leaf and all parent links beneath the task's owned
	// staging directory, not merely the downloader-wide configured root.
	c.scopeMu.Lock()
	scope := c.operationScope
	c.scopeMu.Unlock()
	if scope == "" {
		return downloader.Manifest{}, false
	}
	taskRoot, exists, err := c.findTaskDirectory(ctx, c.directoryID, "omc-"+scope)
	if err != nil || !exists {
		return downloader.Manifest{}, false
	}
	root, err := c.itemWithinDirectory(ctx, outputID, taskRoot.ID)
	if err != nil {
		return downloader.Manifest{}, false
	}
	return downloader.ObserveRequiredOutput(ctx, c.driver, root.ID, plan)
}

var _ downloader.OfflineLifecycleClient = (*Client)(nil)
var _ downloader.OfflineRequiredBinder = (*Client)(nil)

func (c *Client) StopOfflineKeepOutput(ctx context.Context, id string) error {
	return c.Cancel(ctx, id, false)
}

func (c *Client) ObserveOffline(ctx context.Context, id string) (downloader.Task, error) {
	return c.Get(ctx, id)
}
