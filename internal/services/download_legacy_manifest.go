package services

import (
	"context"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
)

type completedManifestRecoveryContext struct{}

func withCompletedManifestRecovery(ctx context.Context) context.Context {
	return context.WithValue(ctx, completedManifestRecoveryContext{}, true)
}
func legacyCompletedManifestAccess(ctx context.Context, task models.DownloadTask) bool {
	allowed, _ := ctx.Value(completedManifestRecoveryContext{}).(bool)
	return allowed && !task.ContentSelectionRequired && task.BTSelectionGroupID == "" && isCompletedRecognitionRecovery(task) && task.ProviderTaskID != ""
}

// An unreadable pre-selection source does not grant normal provider access.
// The only legacy escape is one already-completed task's exact manifest read.
type legacyCompletedManifestClient struct {
	downloadpkg.Client
	manifest   downloadpkg.ManifestClient
	providerID string
}

func bindLegacyCompletedManifest(client downloadpkg.Client, task models.DownloadTask) (downloadpkg.Client, error) {
	manifest, ok := client.(downloadpkg.ManifestClient)
	if !ok {
		return nil, appError("download_completion_manifest_unavailable", "已完成下载的文件清单不可用", nil)
	}
	return &legacyCompletedManifestClient{Client: client, manifest: manifest, providerID: task.ProviderTaskID}, nil
}
func (c *legacyCompletedManifestClient) Manifest(ctx context.Context, id string) (downloadpkg.Manifest, error) {
	if id != c.providerID {
		return downloadpkg.Manifest{}, downloadpkg.Error("download_completion_manifest_read_only", false, nil)
	}
	return c.manifest.Manifest(ctx, id)
}
func (c *legacyCompletedManifestClient) Test(context.Context) (downloadpkg.Health, error) {
	return downloadpkg.Health{}, downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
func (c *legacyCompletedManifestClient) Submit(context.Context, downloadpkg.SubmitRequest) (downloadpkg.Task, error) {
	return downloadpkg.Task{}, downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
func (c *legacyCompletedManifestClient) Get(context.Context, string) (downloadpkg.Task, error) {
	return downloadpkg.Task{}, downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
func (c *legacyCompletedManifestClient) Cancel(context.Context, string, bool) error {
	return downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
func (c *legacyCompletedManifestClient) Pause(context.Context, string) error {
	return downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
func (c *legacyCompletedManifestClient) Resume(context.Context, string) error {
	return downloadpkg.Error("download_completion_manifest_read_only", false, nil)
}
