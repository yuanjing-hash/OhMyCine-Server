package services

import (
	"context"
	"errors"
	"path"
	"time"

	"github.com/yuanjing-hash/OhMyCine-Server/internal/models"
	"github.com/yuanjing-hash/OhMyCine-Server/pkg/cloud"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Cancellation may recover an already-confirmed native receipt whose local
// checkpoint failed. This path only queries the original ID; BeginOperation and
// legacy mutation methods are intentionally unreachable.
func (b quarkMediaLibraryStructureBackend) resolveCancelledCursor(ctx context.Context, boundary StructureBoundary) error {
	q := b.execution
	if q == nil || q.state.cancelPermit == nil || !q.state.Cancelled {
		return ErrCatalogFence
	}
	if q.state.QuarkMutation == nil {
		return nil
	}
	wrapped, ok := q.driver.(*quarkTransferDriver)
	if !ok {
		return ErrObservationDeferred
	}
	native, ok := wrapped.backend.(cloud.AsyncMutationDriver)
	if !ok {
		return ErrObservationDeferred
	}
	cursor := *q.state.QuarkMutation
	var parts []string
	switch cursor.Kind {
	case "mkdir":
		parts = []string{cursor.ParentID, cursor.Name}
	case "move":
		parts = []string{cursor.ItemID, cursor.ParentID}
	case "rename":
		parts = []string{cursor.ItemID, cursor.Name}
	default:
		return ErrCatalogFence
	}
	if err := q.service.withCatalogRead(ctx, q.repair.LibraryID, func(tx *gorm.DB, _ *CatalogReader) error {
		return q.service.validateCatalogStructureExecutionTx(tx, q.repair, *q.state, nil, true)
	}); err != nil {
		return err
	}
	id := quarkTransferOperationID(wrapped.taskID, cursor.Kind, parts...)
	result, err := native.QueryOperation(ctx, id)
	// The shared store's exact absence sentinel is affirmative no-dispatch
	// evidence because physical ownership is already quiescent, not in flight.
	if errors.Is(err, cloud.ErrOperationNotFound) {
		return q.finish(ctx, nil)
	}
	if err != nil {
		return err
	}
	if !result.Complete || result.Receipt.ID != id || result.Receipt.Kind != cursor.Kind || result.Receipt.Provider != cloud.ProviderQuark || result.Receipt.State != "confirmed" {
		return ErrObservationDeferred
	}
	if cursor.Kind == "mkdir" {
		if len(result.Items) != 1 {
			return ErrObservationDeferred
		}
		item, err := wrapped.backend.ResolveReadItem(wrapped.operation(ctx, "mkdir", parts...), result.Items[0].ID)
		if err != nil {
			return err
		}
		checked, err := providerItemWithinRoot(ctx, wrapped, item.ID, boundary.Library.ProviderRootID)
		if err != nil {
			return err
		}
		if !checked.IsDir || checked.ParentID != cursor.ParentID || checked.Name != cursor.Name || path.Base(cursor.TargetRelative) != cursor.Name {
			return ErrCatalogFence
		}
		return q.finish(ctx, func(tx *gorm.DB) error {
			return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "repair_id"}, {Name: "target_relative"}}, DoNothing: true}).Create(&models.CatalogStructureDirectoryReceipt{RepairID: q.repair.ID, TargetRelative: cursor.TargetRelative, ParentProviderID: checked.ID, PreparedAt: time.Now().UTC()}).Error
		})
	}
	item, err := providerItemWithinRoot(ctx, wrapped, cursor.ItemID, boundary.Library.ProviderRootID)
	if err != nil {
		return err
	}
	if item.IsDir || cursor.Kind == "move" && item.ParentID != cursor.ParentID || cursor.Kind == "rename" && item.Name != cursor.Name {
		return ErrCatalogFence
	}
	return q.finish(ctx, nil)
}
