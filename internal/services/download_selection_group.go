package services

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/credential"
	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/resourcecontent"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func (s *DownloadService) prepareBTSelectionGroup(owner uint, executor models.Downloader, source downloadSourceEnvelope, root string, input SubmitDownloadInput) (*models.BTSelectionGroup, error) {
	caps, known := s.downloader.registry.Capabilities(executor.Type)
	if !known || !caps.SelectiveFileGroups || source.ContentSelection == nil || source.SelectionMode != "files" {
		return nil, nil
	}
	if !filepath.IsAbs(root) {
		return nil, appError(CodeInvalidRequest, "精确下载需要受管暂存根目录", nil)
	}
	config := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", executor.ID, executor.Type, executor.BaseURL, executor.UsernameCiphertext, executor.PasswordCiphertext)
	if normalizeExecutionLocation(executor.ExecutionLocation) == models.NodeLocationRemote {
		binding, err := nodeDownloaderBinding(s.db, executor)
		if err != nil {
			return nil, err
		}
		config += fmt.Sprintf("\x00%s\x00%d\x00%s\x00%s", binding.NodeID, binding.Revision, binding.DownloaderSaveRoot, binding.NodeMountRoot)
	}
	// Stable trusted provenance, not a short-lived query claim or a target
	// library/profile. Each reference separately retains its frozen route.
	raw, _ := json.Marshal(struct {
		Owner                      uint
		Config, Root               string
		Site                       uint
		Origin, Plugin, Connection string
		Identity                   resourcecontent.Identity
	}{owner, config, filepath.Clean(root), input.SourceSiteID, "bt", input.PluginID, input.PluginConnectionID, source.ContentSelection.Identity})
	scope := fmt.Sprintf("%x", sha256.Sum256(raw))
	generation := uint64(1)
	var previous models.BTSelectionGroup
	err := s.db.Where("scope_digest = ?", scope).Order("generation DESC").First(&previous).Error
	if err == nil {
		generation = previous.Generation
		if previous.CleanupState == "completed" {
			generation++
		} else if previous.CleanupState != "" {
			return nil, downloadpkg.Error("downloader_selection_group_cleanup_pending", true, nil)
		}
	} else if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", scope, generation))))
	now := time.Now().UTC()
	return &models.BTSelectionGroup{ID: digest, ScopeDigest: scope, Generation: generation, OwnerID: owner, DownloaderID: executor.ID, Workspace: filepath.Join(root, ".omc-bt", digest), KnownDigestsJSON: "[]", CreatedAt: now, UpdatedAt: now}, nil
}

func attachBTSelectionRef(tx *gorm.DB, group *models.BTSelectionGroup, task models.DownloadTask, source downloadSourceEnvelope, stores ...*credential.Store) error {
	if group == nil {
		return nil
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(group).Error; err != nil {
		return err
	}
	var current models.BTSelectionGroup
	if err := tx.First(&current, "id = ?", group.ID).Error; err != nil {
		return err
	}
	if current.OwnerID != task.OwnerID || current.DownloaderID != group.DownloaderID || current.Workspace != group.Workspace || task.BTSelectionGroupID != current.ID {
		return appError(CodeConflict, "精确下载的受管来源范围发生冲突", nil)
	}
	digest, err := source.ContentSelection.Digest()
	if err != nil {
		return err
	}
	var prior models.BTSelectionRef
	if err := tx.First(&prior, "task_id = ?", task.ID).Error; err == nil {
		if prior.GroupID != current.ID || prior.OwnDigest != digest {
			return downloadpkg.Error("downloader_selection_invalid", false, nil)
		}
		return nil
	} else if err != gorm.ErrRecordNotFound {
		return err
	}
	if current.CleanupState != "" || current.LeaseOwner != "" && current.LeaseUntil != nil && current.LeaseUntil.After(time.Now().UTC()) {
		return downloadpkg.Error("downloader_selection_group_busy", true, nil)
	}
	// Lifetime references are retained authority, including released refs used
	// by cleanup. Admission and task/job creation share this transaction, so
	// the 1025th plan cannot poison an existing group's recovery/control.
	var count int64
	if err := tx.Model(&models.BTSelectionRef{}).Where("group_id = ?", current.ID).Count(&count).Error; err != nil {
		return err
	}
	if count >= 1024 {
		return downloadpkg.Error("downloader_selection_group_limit", false, nil)
	}
	if normalizeExecutionLocation(task.ExecutionLocation) == models.NodeLocationRemote {
		if len(stores) == 0 || stores[0] == nil {
			return downloadpkg.Error("downloader_selection_invalid", false, nil)
		}
		union := *source.ContentSelection
		union.Files = append([]resourcecontent.SelectedFile(nil), union.Files...)
		seen := map[int]bool{}
		for _, file := range union.Files {
			seen[file.Index] = true
		}
		var refs []models.BTSelectionRef
		if err := tx.Where("group_id = ? AND state <> ?", group.ID, "released").Limit(1025).Find(&refs).Error; err != nil {
			return err
		}
		if len(refs) > 1024 {
			return downloadpkg.Error("downloader_selection_group_limit", false, nil)
		}
		for _, ref := range refs {
			var prior models.DownloadTask
			if err := tx.First(&prior, "id = ?", ref.TaskID).Error; err != nil {
				return err
			}
			if prior.Phase == models.DownloadTaskStatusCancelled {
				continue
			}
			plain, err := stores[0].Decrypt(downloadSourcePurpose(prior.ID), prior.SourceCiphertext)
			if err != nil {
				return err
			}
			var original downloadSourceEnvelope
			if json.Unmarshal([]byte(plain), &original) != nil || validateStoredContentSelection(original) != nil || original.ContentSelection == nil {
				return downloadpkg.Error("downloader_selection_invalid", false, nil)
			}
			for _, file := range original.ContentSelection.Files {
				if !seen[file.Index] {
					seen[file.Index] = true
					union.Files = append(union.Files, file)
				}
			}
		}
		if err := validateNodeSelectionWireBudget(downloadpkg.Source{Kind: source.Kind, URL: source.URL, Torrent: source.Torrent, Filename: source.Filename, ContentSelection: source.ContentSelection}, union); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	ref := models.BTSelectionRef{TaskID: task.ID, GroupID: current.ID, OwnDigest: digest, State: "queued", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err = tx.Create(&ref).Error; err != nil {
		return err
	}
	return tx.Model(&models.BTSelectionGroup{}).Where("id = ?", current.ID).Updates(map[string]any{"revision": gorm.Expr("revision + 1"), "updated_at": now}).Error
}

type selectionGroupClient struct {
	downloadpkg.Client
	metadata  downloadpkg.MetadataClient
	selective downloadpkg.SelectiveFilesClient
	groups    downloadpkg.SelectionGroupClient
	service   *DownloaderService
	task      models.DownloadTask
	source    downloadpkg.Source
}
type selectionGroupRemote struct {
	*selectionGroupClient
	reader downloadpkg.RemoteFileReader
}

func (s *DownloaderService) bindBTSelectionGroup(client downloadpkg.Client, record models.Downloader, task models.DownloadTask, source downloadpkg.Source) (downloadpkg.Client, error) {
	if _, err := downloadpkg.ValidateContentSource(source); err != nil {
		return nil, err
	}
	groups, ok := client.(downloadpkg.SelectionGroupClient)
	if !ok {
		return nil, downloadpkg.Error("downloader_selection_group_unsupported", false, nil)
	}
	selected, ok := client.(downloadpkg.SelectiveFilesClient)
	if !ok {
		return nil, downloadpkg.Error("downloader_selective_files_unsupported", false, nil)
	}
	metadata, ok := client.(downloadpkg.MetadataClient)
	if !ok {
		return nil, downloadpkg.Error("downloader_selection_group_unsupported", false, nil)
	}
	bound := &selectionGroupClient{Client: client, metadata: metadata, selective: selected, groups: groups, service: s, task: task, source: source}
	if reader, ok := client.(downloadpkg.RemoteFileReader); ok {
		return &selectionGroupRemote{bound, reader}, nil
	}
	return bound, nil
}

func (c *selectionGroupClient) with(ctx context.Context, intent string, fn func(context.Context, downloadpkg.SelectionContext, downloadpkg.Task) error) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	lease := uuid.NewString()
	var group models.BTSelectionGroup
	for {
		now := time.Now().UTC()
		until := now.Add(30 * time.Second)
		result := c.service.db.WithContext(ctx).Model(&models.BTSelectionGroup{}).Where("id = ? AND (lease_owner = '' OR lease_until IS NULL OR lease_until < ?)", c.task.BTSelectionGroupID, now).Updates(map[string]any{"lease_owner": lease, "lease_epoch": gorm.Expr("lease_epoch + 1"), "lease_until": until})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 1 {
			break
		}
		select {
		case <-ctx.Done():
			return downloadpkg.Error("downloader_selection_group_busy", true, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer c.service.db.Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_owner = ?", c.task.BTSelectionGroupID, lease).Updates(map[string]any{"lease_owner": "", "lease_until": nil})
	var snapshot downloadpkg.SelectionGroup
	err := c.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&group, "id = ? AND lease_owner = ?", c.task.BTSelectionGroupID, lease).Error; err != nil {
			return err
		}
		if group.OwnerID != c.task.OwnerID || group.Workspace != c.task.StagingAbsolutePath {
			return appError(CodeConflict, "精确下载的来源范围已变化", nil)
		}
		if group.CleanupState != "" && intent != "retired" {
			return downloadpkg.Error("downloader_selection_group_cleanup_pending", true, nil)
		}
		var own models.BTSelectionRef
		if err := tx.First(&own, "task_id = ? AND group_id = ?", c.task.ID, group.ID).Error; err != nil {
			return err
		}
		var currentTask models.DownloadTask
		if err := tx.Select("id", "phase").First(&currentTask, "id = ?", c.task.ID).Error; err != nil {
			return err
		}
		if currentTask.Phase == models.DownloadTaskStatusCancelled && intent != "released" && !(intent == "retired" && group.CleanupState != "") {
			return downloadpkg.Error("downloader_selection_reference_released", false, nil)
		}
		digest, _ := c.source.ContentSelection.Digest()
		if digest != own.OwnDigest {
			return downloadpkg.Error("downloader_selection_invalid", false, nil)
		}
		if own.State == "released" && intent != "released" {
			return downloadpkg.Error("downloader_selection_reference_released", false, nil)
		}
		if intent != "" && own.State != intent {
			r := tx.Model(&models.BTSelectionRef{}).Where("task_id = ? AND revision = ?", own.TaskID, own.Revision).Updates(map[string]any{"state": intent, "revision": gorm.Expr("revision + 1"), "updated_at": time.Now().UTC()})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return downloadpkg.Error("downloader_selection_group_busy", true, nil)
			}
			if err := tx.Model(&group).UpdateColumn("revision", gorm.Expr("revision + 1")).Error; err != nil {
				return err
			}
			group.Revision++
		}
		var refs []models.BTSelectionRef
		if err := tx.Where("group_id = ? AND state <> ?", group.ID, "released").Order("task_id").Limit(1025).Find(&refs).Error; err != nil {
			return err
		}
		if len(refs) > 1024 {
			return downloadpkg.Error("downloader_selection_group_limit", false, nil)
		}
		union := resourcecontent.Selection{Version: 1, Identity: c.source.ContentSelection.Identity}
		byIndex := map[int]resourcecontent.SelectedFile{}
		resume := false
		for _, ref := range refs {
			var task models.DownloadTask
			if err := tx.First(&task, "id = ? AND bt_selection_group_id = ? AND owner_id = ?", ref.TaskID, group.ID, group.OwnerID).Error; err != nil {
				return err
			}
			// A cancelled task is no longer a source consumer even if its control
			// RPC was interrupted. Its old context cannot resurrect that ref.
			if task.Phase == models.DownloadTaskStatusCancelled {
				continue
			}
			plain, err := c.service.credentials.Decrypt(downloadSourcePurpose(task.ID), task.SourceCiphertext)
			if err != nil {
				return err
			}
			var source downloadSourceEnvelope
			if json.Unmarshal([]byte(plain), &source) != nil || validateStoredContentSelection(source) != nil || source.ContentSelection == nil {
				return downloadpkg.Error("downloader_selection_invalid", false, nil)
			}
			d, _ := source.ContentSelection.Digest()
			if d != ref.OwnDigest || source.ContentSelection.Identity != union.Identity {
				return downloadpkg.Error("downloader_selection_invalid", false, nil)
			}
			for _, file := range source.ContentSelection.Files {
				if old, ok := byIndex[file.Index]; ok && old != file {
					return downloadpkg.Error("downloader_selection_mapping_invalid", false, nil)
				}
				byIndex[file.Index] = file
			}
			resume = resume || ref.State == "running"
		}
		for _, file := range byIndex {
			union.Files = append(union.Files, file)
		}
		sort.Slice(union.Files, func(i, j int) bool { return union.Files[i].Index < union.Files[j].Index })
		empty := len(union.Files) == 0
		if empty {
			union = *c.source.ContentSelection
		}
		digest, _ = union.Digest()
		known := []string{}
		_ = json.Unmarshal([]byte(group.KnownDigestsJSON), &known)
		for _, d := range []string{group.AppliedDigest, group.PendingDigest, digest} {
			if d != "" {
				found := false
				for _, old := range known {
					found = found || d == old
				}
				if !found {
					known = append(known, d)
				}
			}
		}
		if len(known) > 64 {
			return downloadpkg.Error("downloader_selection_group_reconcile_required", true, nil)
		}
		raw, _ := json.Marshal(known)
		if err := tx.Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_owner = ?", group.ID, lease).Updates(map[string]any{"pending_digest": digest, "known_digests_json": string(raw)}).Error; err != nil {
			return err
		}
		snapshot = downloadpkg.SelectionGroup{Version: 1, ID: group.ID, Tag: "omc-group-" + group.ID[:32], Workspace: group.Workspace, Revision: group.Revision, Epoch: group.LeaseEpoch, PreviousDigests: known, Union: union, Resume: resume, Empty: empty}
		return nil
	})
	if err != nil {
		return err
	}
	manifest, err := downloadpkg.ValidateContentSource(c.source)
	if err != nil {
		return err
	}
	check := func() error {
		var n int64
		err := c.service.db.WithContext(ctx).Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_owner = ? AND lease_epoch = ? AND revision = ? AND lease_until > ?", group.ID, lease, snapshot.Epoch, snapshot.Revision, time.Now().UTC()).Count(&n).Error
		if err != nil {
			return err
		}
		if n != 1 {
			return downloadpkg.Error("downloader_selection_group_busy", true, nil)
		}
		if intent != "released" && intent != "retired" {
			var current models.DownloadTask
			if err := c.service.db.WithContext(ctx).Select("phase").First(&current, "id = ?", c.task.ID).Error; err != nil {
				return err
			}
			if current.Phase == models.DownloadTaskStatusCancelled {
				return downloadpkg.Error("downloader_selection_reference_released", false, nil)
			}
		}
		return nil
	}
	if err = check(); err != nil {
		return err
	}
	ctx = downloadpkg.WithSelectionFence(ctx, check)
	provider := downloadpkg.Task{ID: group.ProviderTaskID}
	if group.CleanupState == "" {
		provider, err = c.groups.ApplySelectionGroup(ctx, downloadpkg.SubmitRequest{Source: c.source, SavePath: group.Workspace, Tag: c.task.ProviderTag}, snapshot)
	}
	if err != nil {
		return err
	}
	if group.ProviderTaskID != "" && group.ProviderTaskID != provider.ID {
		return downloadpkg.Error("downloader_selection_identity_mismatch", false, nil)
	}
	if err = check(); err != nil {
		return err
	}
	digest, _ := snapshot.Union.Digest()
	raw, _ := json.Marshal([]string{digest})
	r := c.service.db.WithContext(ctx).Model(&models.BTSelectionGroup{}).Where("id = ? AND lease_owner = ? AND lease_epoch = ? AND revision = ?", group.ID, lease, snapshot.Epoch, snapshot.Revision).Updates(map[string]any{"provider_task_id": provider.ID, "applied_revision": snapshot.Revision, "applied_digest": digest, "pending_digest": "", "known_digests_json": string(raw), "updated_at": time.Now().UTC()})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return downloadpkg.Error("downloader_selection_group_busy", true, nil)
	}
	binding := downloadpkg.SelectionContext{Tag: c.task.ProviderTag, Selection: *c.source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile, Group: &snapshot}
	if fn != nil {
		return fn(ctx, binding, provider)
	}
	return nil
}

func (c *selectionGroupClient) Submit(ctx context.Context, r downloadpkg.SubmitRequest) (out downloadpkg.Task, err error) {
	a, _ := c.source.ContentSelection.Digest()
	if r.Source.ContentSelection == nil {
		return out, downloadpkg.Error("downloader_selection_required", false, nil)
	}
	b, _ := r.Source.ContentSelection.Digest()
	if a != b || r.Tag != c.task.ProviderTag || r.SavePath != c.task.StagingAbsolutePath {
		return out, downloadpkg.Error("downloader_selection_invalid", false, nil)
	}
	err = c.with(ctx, "", func(_ context.Context, _ downloadpkg.SelectionContext, t downloadpkg.Task) error { out = t; return nil })
	return
}
func (c *selectionGroupClient) Get(ctx context.Context, id string) (out downloadpkg.Task, err error) {
	err = c.with(ctx, "", func(ctx context.Context, b downloadpkg.SelectionContext, t downloadpkg.Task) error {
		if id != "" && id != t.ID {
			return downloadpkg.Error("downloader_selection_identity_mismatch", false, nil)
		}
		out, err = c.selective.GetSelected(ctx, t.ID, b)
		return err
	})
	return
}
func (c *selectionGroupClient) Manifest(ctx context.Context, id string) (out downloadpkg.Manifest, err error) {
	err = c.with(ctx, "", func(ctx context.Context, b downloadpkg.SelectionContext, t downloadpkg.Task) error {
		if id != "" && id != t.ID {
			return downloadpkg.Error("downloader_selection_identity_mismatch", false, nil)
		}
		out, err = c.selective.ManifestSelected(ctx, t.ID, b)
		return err
	})
	return
}
func (c *selectionGroupClient) Pause(ctx context.Context, id string) error {
	return c.with(ctx, "paused", nil)
}
func (c *selectionGroupClient) Resume(ctx context.Context, id string) error {
	return c.with(ctx, "running", nil)
}
func (c *selectionGroupClient) Cancel(ctx context.Context, id string, remove bool) error {
	if err := c.with(ctx, "released", nil); err != nil {
		return err
	}
	if remove {
		return downloadpkg.Error("downloader_selected_data_delete_unsupported", false, nil)
	}
	return nil
}
func (c *selectionGroupClient) Categories(context.Context) ([]downloadpkg.Category, error) {
	return nil, nil
}
func (c *selectionGroupClient) EnsureCategory(context.Context, string, string) error { return nil }
func (c *selectionGroupClient) UpdateCategory(context.Context, string, string) error { return nil }
func (c *selectionGroupClient) SetCategory(ctx context.Context, id, n, p string) error {
	if !strings.HasPrefix(filepath.Clean(p), filepath.Clean(c.task.StagingAbsolutePath)) {
		return downloadpkg.Error("downloader_selection_group_workspace_conflict", false, nil)
	}
	return nil
}
func (c *selectionGroupClient) RetainManagedTag() bool { return true }
func (c *selectionGroupRemote) RemoteFileChunks(ctx context.Context, m downloadpkg.Manifest, f downloadpkg.File) ([]downloadpkg.RemoteFileChunk, error) {
	return c.reader.RemoteFileChunks(ctx, m, f)
}
func (c *selectionGroupRemote) ReadRemoteFileChunk(ctx context.Context, m downloadpkg.Manifest, f downloadpkg.File, r downloadpkg.RemoteFileChunk) ([]byte, error) {
	return c.reader.ReadRemoteFileChunk(ctx, m, f, r)
}
func (c *selectionGroupRemote) CleanupManagedSource(context.Context) error {
	return downloadpkg.Error("downloader_selection_group_source_retained", false, nil)
}
