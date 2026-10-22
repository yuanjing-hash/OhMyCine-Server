package services

import (
	"context"
	"errors"
	"path"
	"strings"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	cloud "github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud/quark"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const quarkStructurePending = "quark_structure_confirmation_pending"

// A narrow recovery cursor. The encrypted native OperationStore owns the real
// receipt; the structure plan and ordinal checkpoints remain the file authority.
type quarkStructureMutation struct {
	Kind, ItemID, ParentID, Name, TargetRelative string
}

type quarkStructureExecutor struct {
	service   *MediaLibraryStructureService
	repair    models.MediaLibraryStructureRepair
	plan      StructurePlan
	state     *structureCatalogRepairState
	claim     *ClaimedJob
	driver    cloud.Driver
	mutations cloud.MutationDriver
}

type quarkMediaLibraryStructureBackend struct{ execution *quarkStructureExecutor }

func (quarkMediaLibraryStructureBackend) StorageType() string { return models.StorageTypeQuark }
func (quarkMediaLibraryStructureBackend) ValidateRecycle(context.Context, StructureBoundary) error {
	return cloud.Error(quark.CodeUnsupported, false, nil)
}
func (b quarkMediaLibraryStructureBackend) Recycle(ctx context.Context, boundary StructureBoundary, _ []StructureRecycleItem, _ StructureProgress) error {
	return b.ValidateRecycle(ctx, boundary)
}

func (s *MediaLibraryStructureService) quarkStructureDriver(ctx context.Context, repairID string, library models.MediaLibrary, storage models.Storage) (cloud.Driver, cloud.MutationDriver, error) {
	if storage.Type != models.StorageTypeQuark || storage.ConnectionID == nil || s.connections == nil || library.ProviderRootID == "" || repairID == "" {
		return nil, nil, ErrCatalogFence
	}
	connection, driver, err := s.connections.driver(*storage.ConnectionID)
	if err != nil {
		return nil, nil, err
	}
	if connection.Provider != cloud.ProviderQuark || driver.Provider() != cloud.ProviderQuark {
		return nil, nil, ErrCatalogFence
	}
	caps := driver.Capabilities()
	native, ok := driver.(quarkTransferBackend)
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
	managed, err := native.ResolveManagementItem(ctx, root.ID)
	if err != nil {
		return nil, nil, err
	}
	if managed.ID == "" || !managed.IsDir {
		return nil, nil, ErrCatalogFence
	}
	wrapped, err := transferCloudDriver(driver, "structure-repair:"+repairID)
	if err != nil {
		return nil, nil, err
	}
	mutation, ok := wrapped.(cloud.MutationDriver)
	if !ok {
		return nil, nil, ErrCatalogInvalid
	}
	return wrapped, mutation, nil
}

func (s *MediaLibraryStructureService) bindQuarkStructureBackend(ctx context.Context, repair models.MediaLibraryStructureRepair, plan StructurePlan, state *structureCatalogRepairState, claim *ClaimedJob, boundary StructureBoundary) (MediaLibraryStructureBackend, error) {
	if state.Version != 1 || len(plan.RecycleItems) != 0 {
		return nil, cloud.Error(quark.CodeUnsupported, false, nil)
	}
	for _, item := range plan.Items {
		if item.AllowProviderRootSource || item.ProviderID == "" || item.Size < 0 || safeStructurePath(item.SourceRelative) != item.SourceRelative || safeStructurePath(item.TargetRelative) != item.TargetRelative || (item.Kind != "video" && item.Kind != "sidecar") {
			return nil, ErrCatalogFence
		}
	}
	driver, mutation, err := s.quarkStructureDriver(ctx, repair.ID, boundary.Library, boundary.Storage)
	if err != nil {
		return nil, err
	}
	return quarkMediaLibraryStructureBackend{execution: &quarkStructureExecutor{service: s, repair: repair, plan: plan, state: state, claim: claim, driver: driver, mutations: mutation}}, nil
}

func (q *quarkStructureExecutor) begin(ctx context.Context, cursor quarkStructureMutation) error {
	if q.state.QuarkMutation != nil {
		if *q.state.QuarkMutation != cursor {
			return ErrCatalogFence
		}
		return q.service.withCatalogRead(ctx, q.repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
			return q.service.validateCatalogStructureExecutionTx(tx, q.repair, *q.state, q.claim, true)
		})
	}
	next := *q.state
	next.QuarkMutation, next.ObservationHoldVersion = &cursor, 1
	err := q.service.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		if err := q.service.validateCatalogStructureExecutionTx(tx, q.repair, *q.state, q.claim, true); err != nil {
			return err
		}
		return persistCatalogStructureStateTx(tx, q.repair.ID, next, nil)
	})
	if err == nil {
		*q.state = next
	}
	return err
}

func (q *quarkStructureExecutor) finish(ctx context.Context, write func(*gorm.DB) error) error {
	next := *q.state
	next.QuarkMutation = nil
	err := q.service.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
		if err := q.service.validateCatalogStructureExecutionTx(tx, q.repair, *q.state, q.claim, true); err != nil {
			return err
		}
		if write != nil {
			if err := write(tx); err != nil {
				return err
			}
		}
		return persistCatalogStructureStateTx(tx, q.repair.ID, next, nil)
	})
	if err == nil {
		*q.state = next
	}
	return err
}

func (b quarkMediaLibraryStructureBackend) prepareDirectories(ctx context.Context, boundary StructureBoundary, plan StructurePlan) (map[string]string, error) {
	q := b.execution
	if q == nil {
		return nil, ErrCatalogInvalid
	}
	root := boundary.Library.ProviderRootID
	parents := map[string]string{"": root, ".": root}
	for _, item := range plan.Items {
		relative := path.Dir(item.TargetRelative)
		if relative == "." {
			continue
		}
		parent, walked := root, ""
		for _, segment := range strings.Split(relative, "/") {
			walked = path.Join(walked, segment)
			if cached := parents[walked]; cached != "" {
				parent = cached
				continue
			}
			if err := validateStructureMutation(boundary); err != nil {
				return nil, err
			}
			var receipt models.CatalogStructureDirectoryReceipt
			err := q.service.db.WithContext(ctx).First(&receipt, "repair_id = ? AND target_relative = ?", q.repair.ID, walked).Error
			child := receipt.ParentProviderID
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, err
			}
			cursor := quarkStructureMutation{Kind: "mkdir", ParentID: parent, Name: segment, TargetRelative: walked}
			resume := q.state.QuarkMutation != nil && q.state.QuarkMutation.Kind == "mkdir"
			if resume && *q.state.QuarkMutation != cursor {
				// Earlier ancestors have durable confirmed receipts; a pending
				// later directory cannot be replaced by finding its name in TV.
				if child == "" {
					return nil, ErrCatalogFence
				}
				resume = false
			}
			if child == "" && !resume {
				child, err = providerChildID(ctx, q.driver, parent, segment)
				if err != nil {
					return nil, err
				}
			}
			created := child == "" || resume
			if created {
				if err := q.begin(ctx, cursor); err != nil {
					return nil, err
				}
				if err := validateStructureMutation(boundary); err != nil {
					return nil, err
				}
				output, err := q.mutations.CreateDirectory(ctx, parent, segment)
				if err != nil {
					return nil, q.operationFailure(ctx, err)
				}
				child = output.ID
			}
			stat, err := providerItemWithinRoot(ctx, q.driver, child, root)
			if err != nil {
				return nil, err
			}
			if child == "" || !stat.IsDir || stat.ParentID != parent || stat.Name != segment {
				return nil, ErrCatalogFence
			}
			write := func(tx *gorm.DB) error {
				return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "repair_id"}, {Name: "target_relative"}}, DoNothing: true}).Create(&models.CatalogStructureDirectoryReceipt{RepairID: q.repair.ID, TargetRelative: walked, ParentProviderID: child, PreparedAt: time.Now().UTC()}).Error
			}
			if created {
				err = q.finish(ctx, write)
			} else {
				err = q.service.structureCatalogWriteTx(ctx, func(tx *gorm.DB) error {
					if err := q.service.validateCatalogStructureExecutionTx(tx, q.repair, *q.state, q.claim, true); err != nil {
						return err
					}
					return write(tx)
				})
			}
			if err != nil {
				return nil, err
			}
			parents[walked], parent = child, child
		}
	}
	return parents, nil
}

func (b quarkMediaLibraryStructureBackend) Apply(ctx context.Context, boundary StructureBoundary, items []StructurePlanItem, progress StructureProgress) error {
	q := b.execution
	if q == nil || len(items) != 1 {
		return ErrCatalogInvalid
	}
	item := items[0]
	root := boundary.Library.ProviderRootID
	targetParent := boundary.preparedParents[path.Dir(item.TargetRelative)]
	if targetParent == "" {
		return ErrCatalogFence
	}
	current, err := providerItemWithinRoot(ctx, q.driver, item.ProviderID, root)
	if err != nil {
		return err
	}
	if current.IsDir || current.Size != item.Size {
		return ErrCatalogFence
	}
	sourceParent, err := newProviderStructureDirectoryIndex(ctx, q.driver, root).directoryID(path.Dir(item.SourceRelative))
	if err != nil {
		return err
	}
	sourceName, targetName := path.Base(item.SourceRelative), path.Base(item.TargetRelative)
	if current.ParentID != sourceParent && current.ParentID != targetParent || current.Name != sourceName && current.Name != targetName {
		return ErrCatalogFence
	}
	// Before the first file operation, reject a source changed since planning.
	// A resumed native receipt or already-moved item has its own result checks;
	// provider-side rename/move may legitimately change its modification time.
	if q.state.QuarkMutation == nil && current.ParentID == sourceParent && current.Name == sourceName && item.ModifiedAtUnixNano != 0 && current.ModifiedAt.UTC().UnixNano() != item.ModifiedAtUnixNano {
		return ErrCatalogFence
	}
	conflict, err := providerChildID(ctx, q.driver, targetParent, targetName)
	if err != nil {
		return err
	}
	if conflict != "" && conflict != item.ProviderID {
		return errStructureConflict
	}
	for _, operation := range []quarkStructureMutation{{Kind: "move", ItemID: item.ProviderID, ParentID: targetParent, TargetRelative: item.TargetRelative}, {Kind: "rename", ItemID: item.ProviderID, Name: targetName, TargetRelative: item.TargetRelative}} {
		resume := q.state.QuarkMutation != nil
		if resume && *q.state.QuarkMutation != operation {
			if operation.Kind == "move" && q.state.QuarkMutation.Kind == "rename" && q.state.QuarkMutation.ItemID == item.ProviderID && current.ParentID == targetParent {
				continue
			}
			return ErrCatalogFence
		}
		needed := operation.Kind == "move" && current.ParentID != targetParent || operation.Kind == "rename" && current.Name != targetName
		if !needed && !resume {
			continue
		}
		if err := q.begin(ctx, operation); err != nil {
			return err
		}
		if err := validateStructureMutation(boundary); err != nil {
			return err
		}
		if operation.Kind == "move" {
			err = q.mutations.Move(ctx, item.ProviderID, targetParent)
		} else {
			err = q.mutations.Rename(ctx, item.ProviderID, targetName)
		}
		if err != nil {
			return q.operationFailure(ctx, err)
		}
		current, err = providerItemWithinRoot(ctx, q.driver, item.ProviderID, root)
		if err != nil {
			return err
		}
		if current.IsDir || current.Size != item.Size || current.ParentID != targetParent || operation.Kind == "rename" && current.Name != targetName {
			return ErrCatalogFence
		}
		if err := q.finish(ctx, nil); err != nil {
			return err
		}
	}
	if current.ParentID != targetParent || current.Name != targetName {
		return ErrCatalogFence
	}
	if progress != nil {
		return progress(1, 1)
	}
	return nil
}

func (q *quarkStructureExecutor) operationFailure(ctx context.Context, err error) error {
	var unsubmitted *quark.OperationNotSubmittedError
	if errors.As(err, &unsubmitted) {
		if saveErr := q.finish(ctx, nil); saveErr != nil {
			return saveErr
		}
		if code, _ := cloud.ErrorInfo(err); code == cloud.CodeConflict {
			return errStructureConflict
		}
	}
	return err
}

// A synchronous structure repair is owned by its original transfer caller.
// Preserve the actual wait contract instead of turning it into a failed import.
type structureRepairWaitError struct{ result WorkerResult }

func (e *structureRepairWaitError) Error() string { return e.result.ErrorCode }
func structureRepairResultError(result WorkerResult) error {
	if result.Wait != nil || result.RetryAt != nil {
		return &structureRepairWaitError{result: result}
	}
	if result.ErrorCode != "" {
		return appError(result.ErrorCode, result.ErrorMessage, nil)
	}
	return nil
}

func quarkStructureWaitResult(err error) (WorkerResult, bool) {
	var provider *cloud.ProviderError
	if !errors.As(err, &provider) {
		return WorkerResult{}, false
	}
	result := WorkerResult{ErrorCode: provider.Code, ErrorMessage: "夸克目录修复尚未完成，已保留原操作记录"}
	switch provider.Code {
	case quark.CodePending:
		result.ErrorCode, result.ErrorMessage = quarkStructurePending, "夸克已接受目录修复操作，正在核对原操作结果"
	case cloud.CodeMutationUnknown, quark.CodeAuth:
		if provider.Code == quark.CodeAuth {
			result.ErrorMessage = "夸克授权已失效，请恢复原账号授权后核对目录修复"
		}
		result.Wait = &WaitForAction{ActionType: "quark_structure_reconcile", Prompt: result.ErrorMessage, Options: []string{"重新核对"}}
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
	return result, true
}

// Verify the final TV identities rather than treating a management ACK as a
// catalog fact. Directory receipts are bounded by the frozen plan and include
// every prepared ancestor, including directories created before cancellation.
func (b quarkMediaLibraryStructureBackend) confirmedFacts(ctx context.Context, boundary StructureBoundary, plan StructurePlan, facts *CatalogFactBatch) (quarkStructureEvidence, error) {
	q := b.execution
	e := quarkStructureEvidence{Directories: map[string]string{".": boundary.Library.ProviderRootID}}
	if q == nil || q.state.QuarkMutation != nil {
		return e, ErrObservationDeferred
	}
	var receipts []models.CatalogStructureDirectoryReceipt
	if err := q.service.db.WithContext(ctx).Where("repair_id = ?", q.repair.ID).Order("target_relative").Limit(CatalogMaxDeltaRows + 1).Find(&receipts).Error; err != nil {
		return e, err
	}
	if len(receipts) > CatalogMaxDeltaRows {
		return e, ErrCatalogBudget
	}
	for _, receipt := range receipts {
		item, err := providerItemWithinRoot(ctx, q.driver, receipt.ParentProviderID, boundary.Library.ProviderRootID)
		if err != nil {
			return e, err
		}
		if !item.IsDir || item.Name != path.Base(receipt.TargetRelative) || item.ParentID != e.Directories[path.Dir(receipt.TargetRelative)] {
			return e, ErrCatalogFence
		}
		e.Directories[receipt.TargetRelative] = item.ID
		e.Facts = append(e.Facts, item)
	}
	byID := map[string]cloud.Item{}
	for _, planned := range plan.Items {
		item, err := providerItemWithinRoot(ctx, q.driver, planned.ProviderID, boundary.Library.ProviderRootID)
		if err != nil {
			return e, err
		}
		if item.IsDir || item.Size != planned.Size || item.Name != path.Base(planned.TargetRelative) || item.ParentID != e.Directories[path.Dir(planned.TargetRelative)] {
			return e, ErrCatalogFence
		}
		e.Facts = append(e.Facts, item)
		byID[item.ID] = item
	}
	if _, _, err := quarkStructureObservationFacts(boundary.Library.ProviderRootID, plan, e); err != nil {
		return e, err
	}
	for i := range facts.Entries {
		item, ok := byID[facts.Entries[i].ProviderID]
		if !ok || facts.Entries[i].Tombstone {
			return e, ErrCatalogFence
		}
		facts.Entries[i].ModifiedAt = item.ModifiedAt
	}
	for i := range facts.SourceAssets {
		item, ok := byID[facts.SourceAssets[i].ProviderID]
		if !ok || facts.SourceAssets[i].Tombstone {
			return e, ErrCatalogFence
		}
		facts.SourceAssets[i].ModifiedAt = item.ModifiedAt
		facts.SourceAssets[i].ParentProviderID = item.ParentID
	}
	return e, nil
}
