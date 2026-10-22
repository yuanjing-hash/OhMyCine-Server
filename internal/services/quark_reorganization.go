package services

import (
	"context"
	"errors"
	"path"
	"sort"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
)

const quarkReorganizationPending = "quark_reorganization_confirmation_pending"
const quarkReorganizationRecheck = "重新核对"

func reorganizationSourceReadError(storage models.Storage, err error) error {
	if storage.Type == models.StorageTypeQuark {
		return err
	}
	return ErrCatalogFence // preserve the established 115 boundary contract
}

// This is a recovery cursor, not an alternate operation receipt. The connection
// driver owns the encrypted, frozen-route receipt. Persist the cursor BEFORE
// dispatch so a lost acknowledgement never adopts a same-name directory or
// skips reconciliation merely because a move is already visible through TV.
type quarkReorganizationMutation struct {
	Kind, ItemID, ParentID, Name, DirectoryPath string
}

func (s *MediaReorganizationService) reorganizationCloudDriver(ctx context.Context, library models.MediaLibrary, storage models.Storage, taskID string) (cloud.Driver, cloud.MutationDriver, error) {
	if storage.ConnectionID == nil || s.connections == nil || library.ProviderRootID == "" {
		return nil, nil, ErrCatalogFence
	}
	connection, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return nil, nil, err
	}
	if storage.Type == models.StorageTypePan115 && connection.Provider == cloud.ProviderPan115 {
		mutation, ok := driver.(cloud.MutationDriver)
		if !ok {
			return nil, nil, ErrCatalogInvalid
		}
		return driver, mutation, nil
	}
	if storage.Type != models.StorageTypeQuark || connection.Provider != cloud.ProviderQuark || driver.Provider() != cloud.ProviderQuark {
		return nil, nil, ErrCatalogInvalid
	}
	caps := driver.Capabilities()
	backend, ok := driver.(quarkTransferBackend)
	if !ok || !caps.DirectoryList || !caps.CreateDirectory || !caps.Move || !caps.Rename {
		return nil, nil, cloud.Error(quark.CodeUnsupported, false, nil)
	}
	root, err := providerItemWithinRoot(ctx, driver, library.ProviderRootID, storage.RootPath)
	if err != nil {
		return nil, nil, err
	}
	if !root.IsDir {
		return nil, nil, ErrCatalogFence
	}
	// Global capabilities alone do not prove that this particular root has a
	// TV/management shared-space proof. This is a read-only mapping check.
	managed, err := backend.ResolveManagementItem(ctx, root.ID)
	if err != nil {
		return nil, nil, err
	}
	if managed.ID == "" || !managed.IsDir {
		return nil, nil, ErrCatalogFence
	}
	if taskID == "" {
		return driver, nil, nil
	} // preview never exposes a writer
	wrapped, err := transferCloudDriver(driver, "reorganization:"+taskID)
	if err != nil {
		return nil, nil, err
	}
	mutation, ok := wrapped.(cloud.MutationDriver)
	if !ok {
		return nil, nil, ErrCatalogInvalid
	}
	return wrapped, mutation, nil
}

func rememberQuarkReorganizationFact(state *reorganizationState, item cloud.Item) {
	if state.Catalog.VerifiedFacts == nil {
		state.Catalog.VerifiedFacts = map[string]ObservationFact{}
	}
	state.Catalog.VerifiedFacts[item.ID] = ObservationFact{ID: item.ID, ParentID: item.ParentID, Name: item.Name, IsDir: item.IsDir, Size: item.Size, ModifiedAt: item.ModifiedAt, ContentVersion: item.Revision, Digest: item.SHA1}
}

func quarkReorganizationFacts(state reorganizationState) []cloud.Item {
	ids := make([]string, 0, len(state.Catalog.VerifiedFacts))
	for id := range state.Catalog.VerifiedFacts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]cloud.Item, 0, len(ids))
	for _, id := range ids {
		f := state.Catalog.VerifiedFacts[id]
		out = append(out, cloud.Item{ID: f.ID, ParentID: f.ParentID, Name: f.Name, IsDir: f.IsDir, Size: f.Size, ModifiedAt: f.ModifiedAt, Revision: f.ContentVersion, SHA1: f.Digest})
	}
	return out
}

// File mutations can update their parent directory's own metadata. Refresh the
// exact known parents after the last mutation; do not claim a complete listing.
func (w *MediaReorganizationWorker) verifyQuarkReorganizationDirectories(ctx context.Context, task models.MediaReorganizationTask, library models.MediaLibrary, storage models.Storage, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob) error {
	if storage.Type != models.StorageTypeQuark {
		return nil
	}
	driver, _, err := w.service.reorganizationCloudDriver(ctx, library, storage, task.ID)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(state.Catalog.Directories))
	for relative := range state.Catalog.Directories {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		if err := w.validateCatalogExecution(ctx, task, plan, *state, claim); err != nil {
			return err
		}
		item, err := providerItemWithinRoot(ctx, driver, state.Catalog.Directories[relative], library.ProviderRootID)
		if err != nil {
			return err
		}
		if !item.IsDir || relative != "." && (item.ParentID != state.Catalog.Directories[path.Dir(relative)] || item.Name != path.Base(relative)) {
			return ErrCatalogFence
		}
		rememberQuarkReorganizationFact(state, item)
	}
	return w.persistCatalogReorganizationState(ctx, task, plan, *state, claim)
}

func quarkReorganizationFailure(err error) (string, string, bool) {
	var provider *cloud.ProviderError
	if !errors.As(err, &provider) {
		return "", "", false
	}
	message := ""
	switch provider.Code {
	case quark.CodeAuth:
		message = "夸克授权已失效，请恢复原账号授权后核对任务"
	case quark.CodeProof:
		message = "夸克管理授权与播放授权的共同空间验证不可用"
	case quark.CodeIdentity:
		message = "夸克文件身份核对失败，已保留原操作记录"
	case quark.CodeUnsupported:
		message = "当前夸克连接不具备重新整理所需的文件管理能力"
	case quark.CodeInvalid, quark.CodeIncomplete:
		message = "夸克返回的文件信息不完整，无法确认整理结果"
	case quark.CodePending, cloud.CodeMutationUnknown:
		message = "夸克操作结果尚未确认，已保留原操作记录"
	case quark.CodeLimited, cloud.CodeRateLimited:
		message = "夸克请求受到限流，请在冷却结束后核对原任务"
	case quark.CodeUnavailable, cloud.CodeUnavailable:
		message = "夸克暂时不可用，已保留原操作记录"
	default:
		return "", "", false
	}
	return provider.Code, message, true
}

func (w *MediaReorganizationWorker) beginQuarkReorganizationMutation(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob, next quarkReorganizationMutation) error {
	if plan.StorageType != models.StorageTypeQuark {
		return nil
	}
	if old := state.Catalog.QuarkMutation; old != nil {
		if *old != next {
			return ErrCatalogFence
		}
		return nil
	}
	state.Catalog.QuarkMutation = &next
	state.Catalog.ObservationHoldVersion = 1
	return w.persistCatalogReorganizationState(ctx, task, plan, *state, claim)
}

func (w *MediaReorganizationWorker) finishQuarkReorganizationMutation(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state *reorganizationState, claim ClaimedJob) error {
	if plan.StorageType != models.StorageTypeQuark {
		return nil
	}
	state.Catalog.QuarkMutation = nil
	return w.persistCatalogReorganizationState(ctx, task, plan, *state, claim)
}

// Only typed provider failures are eligible for a cloud retry. Catalog/lease/
// permission failures must never be transformed into network waits.
func (w *MediaReorganizationWorker) quarkReorganizationWait(ctx context.Context, task models.MediaReorganizationTask, plan reorganizationPlan, state reorganizationState, claim ClaimedJob, err error) (WorkerResult, bool) {
	if plan.StorageType != models.StorageTypeQuark || state.Catalog == nil || state.Catalog.Stage == "reconciling" {
		return WorkerResult{}, false
	}
	var provider *cloud.ProviderError
	if !errors.As(err, &provider) {
		return WorkerResult{}, false
	}
	code := provider.Code
	result := WorkerResult{ErrorCode: code, ErrorMessage: "夸克重新整理暂未完成，已保留原操作以供核对"}
	switch code {
	case quark.CodePending:
		if state.Catalog.QuarkMutation == nil {
			return WorkerResult{}, false
		}
		result.ErrorCode = quarkReorganizationPending
		result.ErrorMessage = "夸克已接受操作，正在等待并核对原操作结果"
	case cloud.CodeMutationUnknown:
		if state.Catalog.QuarkMutation == nil {
			return WorkerResult{}, false
		}
		result.ErrorMessage = "夸克操作结果尚不明确，请核对原操作；不会重复提交或采用同名文件"
		result.Wait = &WaitForAction{ActionType: "quark_reorganization_reconcile", Prompt: result.ErrorMessage, Options: []string{quarkReorganizationRecheck}}
	case quark.CodeAuth:
		result.ErrorMessage = "夸克授权已失效，请恢复原账号授权后继续核对"
		result.Wait = &WaitForAction{ActionType: "quark_reorganization_authorization", Prompt: result.ErrorMessage, Options: []string{quarkReorganizationRecheck}}
	case quark.CodeLimited, quark.CodeUnavailable, cloud.CodeRateLimited, cloud.CodeUnavailable:
		if !provider.Retryable {
			return WorkerResult{}, false
		}
	default:
		return WorkerResult{}, false
	}
	if result.Wait == nil {
		delay := 15 * time.Second
		var failure *quark.Failure
		if errors.As(err, &failure) && failure.RetryAfter > delay {
			delay = failure.RetryAfter
		}
		next := time.Now().UTC().Add(delay)
		result.RetryAt = &next
	}
	// Do not mark the domain task failed while the scheduler waits. Rechecking
	// keeps the same job, frozen plan, operation namespace and observation hold.
	e := w.service.libraries.catalogStore.Admission().WithForeground(ctx, func() error {
		return w.service.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if _, e := w.service.queue.verifyLease(tx, claim.Job.ID, claim.LeaseToken); e != nil {
				return e
			}
			return tx.Model(&models.MediaReorganizationTask{}).Where("id=? AND job_id=? AND phase<>?", task.ID, claim.Job.ID, models.MediaReorganizationPhaseCompleted).Updates(map[string]any{"last_error_code": result.ErrorCode, "updated_at": time.Now().UTC()}).Error
		})
	})
	if e != nil {
		return WorkerResult{}, false
	}
	return result, true
}
