package nodeagent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

var errDownloadGroupStale = errors.New("node_selection_group_stale")

// A persisted lease serializes physical mutations, including a late previous
// process. Epoch/revision never decrease and a retry must carry the same
// snapshot. No database transaction spans an executor request.
func (s *Store) beginDownloadGroup(ctx context.Context, server, downloader, task, scope, local string, binding downloadpkg.SelectionContext, group downloadpkg.SelectionGroup, cleanupMode ...bool) (func(string) error, func(), error) {
	if err := group.Validate(binding.Selection); err != nil {
		return nil, nil, err
	}
	raw, _ := json.Marshal(group)
	digest := fmt.Sprintf("%x", sha256.Sum256(raw))
	lease := uuid.NewString()
	binding.Group = nil
	own, _ := json.Marshal(binding)
	for {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO managed_download_groups(paired_server_id,downloader_id,group_id,configuration_scope,workspace,snapshot_json,epoch,revision,snapshot_digest)VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, server, downloader, group.ID, scope, group.Workspace, string(raw), group.Epoch, group.Revision, digest)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}
		var storedScope, workspace, storedDigest, cleanupState string
		var epoch, revision uint64
		err = tx.QueryRowContext(ctx, `SELECT configuration_scope,workspace,epoch,revision,snapshot_digest,cleanup_state FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND group_id=?`, server, downloader, group.ID).Scan(&storedScope, &workspace, &epoch, &revision, &storedDigest, &cleanupState)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}
		if cleanupState != "" && (len(cleanupMode) == 0 || !cleanupMode[0]) {
			_ = tx.Rollback()
			return nil, nil, ErrPlanConflict
		}
		if epoch > group.Epoch || revision > group.Revision {
			_ = tx.Rollback()
			return nil, nil, errDownloadGroupStale
		}
		if storedScope != scope || workspace != group.Workspace || epoch == group.Epoch && storedDigest != digest {
			_ = tx.Rollback()
			return nil, nil, ErrPlanConflict
		}
		now := time.Now().UTC()
		until := now.Add(30 * time.Second)
		r, err := tx.ExecContext(ctx, `UPDATE managed_download_groups SET epoch=?,revision=?,snapshot_json=?,snapshot_digest=?,lease_owner=?,lease_until=? WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND (lease_owner='' OR lease_until IS NULL OR lease_until<?)`, group.Epoch, group.Revision, string(raw), digest, lease, until, server, downloader, group.ID, now)
		if err != nil {
			_ = tx.Rollback()
			return nil, nil, err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			_ = tx.Rollback()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO managed_download_group_refs(paired_server_id,downloader_id,task_id,group_id,binding_json,node_local_root)VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`, server, downloader, task, group.ID, string(own), local)
		var gotGroup, gotBinding, gotLocal string
		if err == nil {
			err = tx.QueryRowContext(ctx, `SELECT group_id,binding_json,node_local_root FROM managed_download_group_refs WHERE paired_server_id=? AND downloader_id=? AND task_id=?`, server, downloader, task).Scan(&gotGroup, &gotBinding, &gotLocal)
		}
		if err != nil || gotGroup != group.ID || gotBinding != string(own) || gotLocal != local {
			_ = tx.Rollback()
			return nil, nil, ErrPlanConflict
		}
		if err = tx.Commit(); err != nil {
			return nil, nil, err
		}
		break
	}
	release := func() {
		_, _ = s.db.ExecContext(context.Background(), `UPDATE managed_download_groups SET lease_owner='',lease_until=NULL WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND lease_owner=?`, server, downloader, group.ID, lease)
	}
	finish := func(provider string) error {
		r, err := s.db.ExecContext(ctx, `UPDATE managed_download_groups SET provider_task_id=? WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=? AND revision=? AND lease_owner=? AND (provider_task_id='' OR provider_task_id=?) AND lease_until>?`, provider, server, downloader, group.ID, group.Epoch, group.Revision, lease, provider, time.Now().UTC())
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n != 1 {
			return ErrPlanConflict
		}
		return nil
	}
	return finish, release, nil
}

func (s *Store) managedGroupDownload(ctx context.Context, server, downloader, task, provider string) (ManagedDownload, downloadpkg.SelectionContext, error) {
	var d ManagedDownload
	var b downloadpkg.SelectionContext
	var own, raw string
	err := s.db.QueryRowContext(ctx, `SELECT r.binding_json,r.node_local_root,g.workspace,g.snapshot_json,g.provider_task_id FROM managed_download_group_refs r JOIN managed_download_groups g ON g.paired_server_id=r.paired_server_id AND g.downloader_id=r.downloader_id AND g.group_id=r.group_id WHERE r.paired_server_id=? AND r.downloader_id=? AND r.task_id=?`, server, downloader, task).Scan(&own, &d.NodeLocalRoot, &d.DownloaderSavePath, &raw, &d.ProviderTaskID)
	if errors.Is(err, sql.ErrNoRows) {
		return d, b, ErrOperationNotFound
	}
	if err != nil {
		return d, b, err
	}
	var group downloadpkg.SelectionGroup
	if json.Unmarshal([]byte(own), &b) != nil || json.Unmarshal([]byte(raw), &group) != nil || group.Validate(b.Selection) != nil || d.ProviderTaskID != provider {
		return d, b, ErrPlanConflict
	}
	b.Group = &group
	d.ServerID = server
	d.DownloaderID = downloader
	d.TaskID = task
	d.Tag = b.Tag
	return d, b, nil
}

func (a *Agent) executeSelectionGroup(ctx context.Context, server string, input nodeprotocol.DownloaderActionRequest, credential nodeprotocol.DownloaderCredential, base downloadpkg.Client, source downloadpkg.Source, local string) (downloadpkg.Task, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	group := input.Group
	if group == nil {
		return downloadpkg.Task{}, ErrPlanConflict
	}
	expected := strings.TrimRight(strings.ReplaceAll(credential.DownloaderSaveRoot, `\`, "/"), "/") + "/.omc-bt/" + group.ID
	actual := strings.ReplaceAll(group.Workspace, `\`, "/")
	if actual != expected {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, nil)
	}
	manifest, err := downloadpkg.ValidateContentSource(source)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	if err = group.Union.ValidateAgainst(manifest); err != nil {
		return downloadpkg.Task{}, ErrPlanConflict
	}
	binding := downloadpkg.SelectionContext{Tag: input.Submit.Tag, Selection: *source.ContentSelection, RootName: manifest.Name, MultiFile: manifest.MultiFile}
	finish, release, err := a.store.beginDownloadGroup(ctx, server, input.DownloaderID, input.TaskID, credential.SourceScope, local, binding, *group)
	if err != nil {
		if errors.Is(err, errDownloadGroupStale) {
			return downloadpkg.Task{}, downloadpkg.Error("downloader_selection_group_busy", true, err)
		}
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	defer release()
	ctx, err = a.store.downloadGroupFence(ctx, server, input.DownloaderID, *group)
	if err != nil {
		return downloadpkg.Task{}, err
	}
	// First execution owns a not-yet-existing child, never an arbitrary remote
	// path. Prepare and inspect it through the Node's verified mount, under the
	// persisted group lease, before qB can follow a pre-existing reparse child.
	if err := downloadpkg.CheckSelectionFence(ctx); err != nil {
		return downloadpkg.Task{}, err
	}
	workspace, err := ensureSecureStorageSourceDirectory(credential.NodeMountRoot, []string{".omc-bt", group.ID})
	if err != nil || !sameCleanPath(workspace, local) {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPathMappingInvalid, false, nil)
	}
	adapter, ok := base.(downloadpkg.SelectionGroupClient)
	if !ok {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	task, err := adapter.ApplySelectionGroup(ctx, downloadpkg.SubmitRequest{Source: source, Tag: input.Submit.Tag, SavePath: input.Submit.SavePath}, *group)
	if err != nil {
		return task, err
	}
	if err = finish(task.ID); err != nil {
		return downloadpkg.Task{}, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	return task, nil
}

func (s *Store) downloadGroupFence(ctx context.Context, server, downloader string, group downloadpkg.SelectionGroup) (context.Context, error) {
	var owner string
	if err := s.db.QueryRowContext(ctx, `SELECT lease_owner FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=? AND revision=?`, server, downloader, group.ID, group.Epoch, group.Revision).Scan(&owner); err != nil || owner == "" {
		return ctx, downloadpkg.Error("downloader_selection_group_busy", true, err)
	}
	check := func() error {
		var n int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND group_id=? AND epoch=? AND revision=? AND lease_owner=? AND lease_until>?`, server, downloader, group.ID, group.Epoch, group.Revision, owner, time.Now().UTC()).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			return downloadpkg.Error("downloader_selection_group_busy", true, nil)
		}
		return nil
	}
	return downloadpkg.WithSelectionFence(ctx, check), nil
}

func (a *Agent) managedDownloadForTask(ctx context.Context, server, downloader, task, provider string) (ManagedDownload, error) {
	managed, _, err := a.store.managedGroupDownload(ctx, server, downloader, task, provider)
	if err == nil {
		return managed, nil
	}
	if !errors.Is(err, ErrOperationNotFound) {
		return ManagedDownload{}, err
	}
	managed, err = a.store.ManagedDownload(ctx, server, downloader, provider)
	if err == nil && managed.TaskID != task {
		return ManagedDownload{}, ErrPlanConflict
	}
	return managed, err
}
