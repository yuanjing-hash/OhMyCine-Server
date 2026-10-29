package nodeagent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	downloadpkg "github.com/yuanjing-hash/OhMyCine-Server/pkg/downloader"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/nodeprotocol"
)

func (s *Store) saveDownloadSelection(ctx context.Context, serverID, downloaderID, taskID, providerID string, binding downloadpkg.SelectionContext) error {
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO managed_download_selections(paired_server_id,downloader_id,task_id,provider_task_id,binding_json)VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, serverID, downloaderID, taskID, providerID, string(raw))
	if err != nil {
		return err
	}
	var stored, storedProvider string
	err = s.db.QueryRowContext(ctx, `SELECT provider_task_id,binding_json FROM managed_download_selections WHERE paired_server_id=? AND downloader_id=? AND task_id=?`, serverID, downloaderID, taskID).Scan(&storedProvider, &stored)
	if err == nil && (storedProvider != providerID || stored != string(raw)) {
		return ErrPlanConflict
	}
	return err
}
func (s *Store) downloadSelection(ctx context.Context, serverID, downloaderID, taskID string) (downloadpkg.SelectionContext, string, error) {
	var raw, provider string
	var binding downloadpkg.SelectionContext
	err := s.db.QueryRowContext(ctx, `SELECT provider_task_id,binding_json FROM managed_download_selections WHERE paired_server_id=? AND downloader_id=? AND task_id=?`, serverID, downloaderID, taskID).Scan(&provider, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return binding, "", ErrOperationNotFound
	}
	if err != nil {
		return binding, "", err
	}
	if json.Unmarshal([]byte(raw), &binding) != nil {
		return binding, "", ErrPlanConflict
	}
	return binding, provider, nil
}

func (a *Agent) bindManagedSelection(ctx context.Context, serverID string, input nodeprotocol.DownloaderActionRequest, client downloadpkg.Client) (downloadpkg.Client, error) {
	if input.ProviderTaskID == "" {
		return client, nil
	}
	if _, binding, err := a.store.managedGroupDownload(ctx, serverID, input.DownloaderID, input.TaskID, input.ProviderTaskID); err == nil {
		if input.Selection != nil {
			given := downloadpkg.SelectionContext{Tag: input.Selection.Tag, Selection: input.Selection.Selection, RootName: input.Selection.RootName, MultiFile: input.Selection.MultiFile}
			own := binding
			own.Group = nil
			a, _ := json.Marshal(own)
			b, _ := json.Marshal(given)
			if string(a) != string(b) || input.Selection.Group == nil || input.Selection.Group.ID != binding.Group.ID {
				return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
			}
		}
		selected, ok := client.(downloadpkg.SelectiveFilesClient)
		if !ok {
			return nil, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
		}
		return &managedSelectedClient{Client: client, MetadataClient: client.(downloadpkg.MetadataClient), selected: selected, binding: binding}, nil
	} else if !errors.Is(err, ErrOperationNotFound) {
		return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	var shared int
	if err := a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_download_groups WHERE paired_server_id=? AND downloader_id=? AND provider_task_id=? AND cleanup_state<>'completed'`, serverID, input.DownloaderID, input.ProviderTaskID).Scan(&shared); err != nil {
		return nil, err
	}
	if shared != 0 {
		return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
	}
	managed, err := a.store.ManagedDownload(ctx, serverID, input.DownloaderID, input.ProviderTaskID)
	if err != nil {
		if input.Selection != nil {
			return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
		}
		return client, nil
	}
	binding, provider, err := a.store.downloadSelection(ctx, serverID, input.DownloaderID, managed.TaskID)
	if errors.Is(err, ErrOperationNotFound) {
		if input.Selection != nil {
			return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
		}
		return client, nil
	}
	if err != nil || managed.TaskID != input.TaskID || provider != input.ProviderTaskID {
		return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, err)
	}
	if input.Selection != nil {
		actual, _ := json.Marshal(binding)
		given, _ := json.Marshal(downloadpkg.SelectionContext{Tag: input.Selection.Tag, Selection: input.Selection.Selection, RootName: input.Selection.RootName, MultiFile: input.Selection.MultiFile})
		if string(actual) != string(given) {
			return nil, downloadpkg.Error(nodeprotocol.ErrorPlanConflict, false, nil)
		}
	}
	// The Server's immutable plan is already persisted and was originally
	// validated against metainfo. Construct a task facade without re-fetching it.
	selected, ok := client.(downloadpkg.SelectiveFilesClient)
	if !ok {
		return nil, downloadpkg.Error(nodeprotocol.ErrorCapabilityMissing, false, nil)
	}
	return &managedSelectedClient{Client: client, MetadataClient: client.(downloadpkg.MetadataClient), selected: selected, binding: binding}, nil
}

type managedSelectedClient struct {
	downloadpkg.Client
	downloadpkg.MetadataClient
	selected downloadpkg.SelectiveFilesClient
	binding  downloadpkg.SelectionContext
}

func (c *managedSelectedClient) Get(ctx context.Context, id string) (downloadpkg.Task, error) {
	return c.selected.GetSelected(ctx, id, c.binding)
}
func (c *managedSelectedClient) Manifest(ctx context.Context, id string) (downloadpkg.Manifest, error) {
	return c.selected.ManifestSelected(ctx, id, c.binding)
}
func (c *managedSelectedClient) Pause(ctx context.Context, id string) error {
	if c.binding.Group != nil {
		return downloadpkg.Error("downloader_selection_group_update_required", false, nil)
	}
	return c.selected.PauseSelected(ctx, id, c.binding)
}
func (c *managedSelectedClient) Resume(ctx context.Context, id string) error {
	if c.binding.Group != nil {
		return downloadpkg.Error("downloader_selection_group_update_required", false, nil)
	}
	return c.selected.ResumeSelected(ctx, id, c.binding)
}
func (c *managedSelectedClient) Cancel(ctx context.Context, id string, remove bool) error {
	if c.binding.Group != nil {
		return downloadpkg.Error("downloader_selection_group_update_required", false, nil)
	}
	return c.selected.CancelSelected(ctx, id, c.binding, remove)
}
func (c *managedSelectedClient) SetCategory(ctx context.Context, id, n, p string) error {
	if c.binding.Group != nil {
		return downloadpkg.Error("downloader_selection_group_workspace_conflict", false, nil)
	}
	if selected, ok := c.MetadataClient.(downloadpkg.SelectiveMetadataClient); ok {
		return selected.SetCategorySelected(ctx, id, c.binding, n, p)
	}
	if _, err := c.Get(ctx, id); err != nil {
		return err
	}
	return c.MetadataClient.SetCategory(ctx, id, n, p)
}
